package firestore

import (
	"context"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Commit applies a batch of writes. Every write the Go client makes — Set,
// Create, Update, Delete, and a WriteBatch — arrives here.
func (s *Service) Commit(ctx context.Context, req *firestorepb.CommitRequest) (*firestorepb.CommitResponse, error) {
	now := timestamppb.New(s.clk.Now())

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.closeTransaction(req.GetTransaction()); err != nil {
		return nil, err
	}

	// Checked before anything is written, so a batch with one bad
	// precondition leaves nothing behind.
	staged := make([]*firestorepb.Document, 0, len(req.GetWrites()))
	transformed := make([][]*firestorepb.Value, 0, len(req.GetWrites()))
	for _, w := range req.GetWrites() {
		doc, results, err := s.stage(ctx, w, now)
		if err != nil {
			return nil, err
		}
		staged = append(staged, doc)
		transformed = append(transformed, results)
	}

	results := make([]*firestorepb.WriteResult, 0, len(staged))
	for i, w := range req.GetWrites() {
		if err := s.apply(ctx, w, staged[i]); err != nil {
			return nil, err
		}
		result := &firestorepb.WriteResult{TransformResults: transformed[i]}
		if staged[i] != nil {
			result.UpdateTime = staged[i].GetUpdateTime()
		}
		results = append(results, result)
	}
	return &firestorepb.CommitResponse{WriteResults: results, CommitTime: now}, nil
}

// stage resolves one write against current state, returning the document to
// store, or nil for a delete. It writes nothing.
func (s *Service) stage(ctx context.Context, w *firestorepb.Write, now *timestamppb.Timestamp) (*firestorepb.Document, []*firestorepb.Value, error) {
	name, err := targetOf(w)
	if err != nil {
		return nil, nil, err
	}
	if err := validDocName(name); err != nil {
		return nil, nil, err
	}

	current, _, exists, err := s.get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if err := checkPrecondition(name, w.GetCurrentDocument(), current, exists); err != nil {
		return nil, nil, err
	}
	if w.GetDelete() != "" {
		return nil, nil, nil
	}

	incoming := w.GetUpdate()
	next := &firestorepb.Document{
		Name:       name,
		Fields:     map[string]*firestorepb.Value{},
		CreateTime: now,
		UpdateTime: now,
	}
	if exists {
		next.CreateTime = current.GetCreateTime()
		// An update mask means "touch only these fields", so the rest of the
		// stored document survives; without one the write replaces it.
		if w.GetUpdateMask() != nil {
			for k, v := range current.GetFields() {
				next.Fields[k] = proto.Clone(v).(*firestorepb.Value)
			}
		}
	}

	if mask := w.GetUpdateMask(); mask != nil {
		// A field named by the mask but absent from the write is a delete.
		// The path is walked, not used as a key: "profile.name" names a field
		// inside profile, and the write carries it nested the same way.
		for _, path := range mask.GetFieldPaths() {
			if v, ok := lookup(incoming.GetFields(), path); ok {
				setPath(next.Fields, path, proto.Clone(v).(*firestorepb.Value))
			} else {
				deletePath(next.Fields, path)
			}
		}
	} else {
		for k, v := range incoming.GetFields() {
			next.Fields[k] = proto.Clone(v).(*firestorepb.Value)
		}
	}

	// Transforms run last, over the document the write produced: an increment
	// applies to what was stored, not to what the caller last read.
	results, err := applyTransforms(next, transformsOf(w), now)
	if err != nil {
		return nil, nil, err
	}
	return next, results, nil
}

// transformsOf collects a write's transforms. The client attaches them to an
// update as updateTransforms, and sends a bare transform as its own write.
func transformsOf(w *firestorepb.Write) []*firestorepb.DocumentTransform_FieldTransform {
	if t := w.GetTransform(); t != nil {
		return t.GetFieldTransforms()
	}
	return w.GetUpdateTransforms()
}

// apply writes what stage resolved.
func (s *Service) apply(ctx context.Context, w *firestorepb.Write, doc *firestorepb.Document) error {
	name, _ := targetOf(w)

	if doc == nil {
		// Deleting an absent document is not an error, as in real Firestore.
		if err := s.kv.Delete(ctx, docKey(name), 0); err != nil {
			return nil
		}
		return nil
	}

	encoded, err := marshal.Marshal(doc)
	if err != nil {
		return status.Errorf(codes.Internal, "encoding document: %v", err)
	}
	_, version, exists, err := s.get(ctx, name)
	if err != nil {
		return err
	}
	var ifVersion uint64
	if exists {
		ifVersion = version
	}
	if _, err := s.kv.Put(ctx, docKey(name), encoded, ifVersion); err != nil {
		return status.Errorf(codes.Aborted, "storing document: %v", err)
	}
	return nil
}

// targetOf names the document a write acts on.
func targetOf(w *firestorepb.Write) (string, error) {
	if name := w.GetDelete(); name != "" {
		return name, nil
	}
	if doc := w.GetUpdate(); doc != nil {
		return doc.GetName(), nil
	}
	if t := w.GetTransform(); t != nil {
		return t.GetDocument(), nil
	}
	return "", status.Error(codes.InvalidArgument, "a write must name a document")
}

// checkPrecondition enforces the exists and update-time guards that make
// Create and a conditional Update safe.
func checkPrecondition(name string, pre *firestorepb.Precondition, current *firestorepb.Document, exists bool) error {
	switch c := pre.GetConditionType().(type) {
	case nil:
		return nil
	case *firestorepb.Precondition_Exists:
		if c.Exists && !exists {
			return status.Errorf(codes.NotFound, "no document to update: %s", name)
		}
		if !c.Exists && exists {
			return status.Errorf(codes.AlreadyExists, "document already exists: %s", name)
		}
	case *firestorepb.Precondition_UpdateTime:
		if !exists || !c.UpdateTime.AsTime().Equal(current.GetUpdateTime().AsTime()) {
			return status.Errorf(codes.FailedPrecondition,
				"the document was modified: %s", name)
		}
	}
	return nil
}

// BatchWrite applies writes independently: one failing does not stop the rest,
// and each gets its own status. BulkWriter arrives here rather than at Commit,
// which is atomic.
func (s *Service) BatchWrite(ctx context.Context, req *firestorepb.BatchWriteRequest) (*firestorepb.BatchWriteResponse, error) {
	now := timestamppb.New(s.clk.Now())

	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]*firestorepb.WriteResult, 0, len(req.GetWrites()))
	statuses := make([]*spb.Status, 0, len(req.GetWrites()))

	for _, w := range req.GetWrites() {
		doc, transformed, err := s.stage(ctx, w, now)
		if err == nil {
			err = s.apply(ctx, w, doc)
		}

		result := &firestorepb.WriteResult{TransformResults: transformed}
		if err == nil && doc != nil {
			result.UpdateTime = doc.GetUpdateTime()
		}
		results = append(results, result)
		statuses = append(statuses, status.Convert(err).Proto())
	}
	return &firestorepb.BatchWriteResponse{WriteResults: results, Status: statuses}, nil
}
