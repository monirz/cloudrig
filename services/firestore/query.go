package firestore

import (
	"context"
	"sort"
	"strings"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// RunQuery answers a structured query. Every Documents() iterator and every
// Where() chain the Go client builds arrives here.
func (s *Service) RunQuery(req *firestorepb.RunQueryRequest, stream firestorepb.Firestore_RunQueryServer) error {
	q := req.GetStructuredQuery()
	if q == nil {
		return status.Error(codes.InvalidArgument, "a query is required")
	}
	if len(q.GetFrom()) != 1 {
		return status.Error(codes.Unimplemented,
			"a query must name exactly one collection; collection-group queries are not supported")
	}
	from := q.GetFrom()[0]
	if from.GetAllDescendants() {
		return status.Error(codes.Unimplemented, "collection-group queries are not supported")
	}

	// Checked before anything is sliced: a negative bound reaches here
	// straight from the request, and a slice would panic on it.
	if q.GetOffset() < 0 {
		return status.Errorf(codes.InvalidArgument, "offset must not be negative, got %d", q.GetOffset())
	}
	if limit := q.GetLimit(); limit != nil && limit.GetValue() < 0 {
		return status.Errorf(codes.InvalidArgument, "limit must not be negative, got %d", limit.GetValue())
	}

	docs, err := s.collection(stream.Context(), req.GetParent(), from.GetCollectionId())
	if err != nil {
		return err
	}

	kept := make([]*firestorepb.Document, 0, len(docs))
	for _, doc := range docs {
		ok, err := matches(doc, q.GetWhere())
		if err != nil {
			return err
		}
		if ok {
			kept = append(kept, doc)
		}
	}

	// Firestore returns only documents that have every ordered field, so a
	// document missing one is not merely sorted oddly: it is not a result.
	kept = withOrderedFields(kept, q.GetOrderBy())
	if err := order(kept, q.GetOrderBy()); err != nil {
		return err
	}
	kept = window(kept, q.GetOffset(), q.GetLimit())

	readTime := timestamppb.New(s.clk.Now())
	for _, doc := range kept {
		if err := stream.Send(&firestorepb.RunQueryResponse{
			Document: doc,
			ReadTime: readTime,
		}); err != nil {
			return err
		}
	}
	return nil
}

// collection reads every document directly under a collection.
//
// The store lists in key order, which is document-name order — the order
// Firestore falls back to when a query names none.
func (s *Service) collection(ctx context.Context, parent, collection string) ([]*firestorepb.Document, error) {
	prefix := collectionPrefix(parent, collection)

	entries, _, err := s.kv.List(ctx, prefix, 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing documents: %v", err)
	}

	out := make([]*firestorepb.Document, 0, len(entries))
	for _, kv := range entries {
		// Subcollections live under a longer path; a query over a collection
		// returns only its own documents.
		if strings.Contains(strings.TrimPrefix(kv.Key, prefix), "/") {
			continue
		}
		var doc firestorepb.Document
		if err := unmarshal.Unmarshal(kv.Val, &doc); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding document: %v", err)
		}
		out = append(out, &doc)
	}
	return out, nil
}

// withOrderedFields drops documents that lack a field the query orders by.
func withOrderedFields(docs []*firestorepb.Document, clauses []*firestorepb.StructuredQuery_Order) []*firestorepb.Document {
	if len(clauses) == 0 {
		return docs
	}

	kept := docs[:0]
	for _, doc := range docs {
		complete := true
		for _, c := range clauses {
			if _, ok := fieldOf(doc, c.GetField().GetFieldPath()); !ok {
				complete = false
				break
			}
		}
		if complete {
			kept = append(kept, doc)
		}
	}
	return kept
}

// order sorts by the query's order-by clauses, leaving name order where the
// clauses tie.
func order(docs []*firestorepb.Document, clauses []*firestorepb.StructuredQuery_Order) error {
	if len(clauses) == 0 {
		return nil
	}

	var failed error
	sort.SliceStable(docs, func(i, j int) bool {
		for _, c := range clauses {
			path := c.GetField().GetFieldPath()
			a, aok := fieldOf(docs[i], path)
			b, bok := fieldOf(docs[j], path)
			if !aok || !bok {
				// Firestore omits documents missing an ordered field; this
				// keeps them in a stable place instead of ordering randomly.
				continue
			}
			cmp := compare(a, b)
			if cmp == 0 {
				continue
			}
			if c.GetDirection() == firestorepb.StructuredQuery_DESCENDING {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	return failed
}

// window applies offset and limit, in that order.
// The bounds are validated by the caller; the guards here are so that a future
// caller cannot turn a bad request into a panic.
func window(docs []*firestorepb.Document, offset int32, limit *wrapperspb.Int32Value) []*firestorepb.Document {
	if offset > 0 {
		if int(offset) >= len(docs) {
			return nil
		}
		docs = docs[offset:]
	}
	if limit != nil && limit.GetValue() >= 0 && int(limit.GetValue()) < len(docs) {
		docs = docs[:limit.GetValue()]
	}
	return docs
}
