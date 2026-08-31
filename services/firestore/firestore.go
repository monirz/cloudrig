// Package firestore is the Cloud Firestore emulation.
//
// gRPC only, like Pub/Sub: the Go client speaks no REST. Two RPCs carry the
// whole document API — the client routes every write through Commit and every
// read by reference through BatchGetDocuments, so the per-document RPCs the
// API documents are never called.
package firestore

import (
	"context"
	"strings"
	"sync"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// Service holds documents.
type Service struct {
	firestorepb.UnimplementedFirestoreServer

	kv  store.Store
	clk clock.Clock

	// mu serialises a commit, which must apply as a unit: a batch that half
	// applies is worse than one that fails.
	mu sync.Mutex
}

// New wires a service.
func New(kv store.Store, clk clock.Clock) *Service {
	return &Service{kv: kv, clk: clk}
}

// docKey addresses a document by its resource name, which already carries the
// project and database.
func docKey(name string) string { return "fs/d/" + name }

// collectionPrefix is the key prefix holding a collection's documents. A
// document name is the collection's name plus one more segment.
func collectionPrefix(parent, collection string) string {
	return docKey(parent + "/" + collection + "/")
}

// validDocName checks the shape the API requires: a document lives under
// .../documents/ and has an even number of segments after it.
func validDocName(name string) error {
	const marker = "/documents/"
	i := strings.Index(name, marker)
	if i < 0 {
		return status.Errorf(codes.InvalidArgument, "invalid document name %q", name)
	}
	rest := strings.Split(strings.TrimPrefix(name[i+len(marker):], "/"), "/")
	if len(rest) == 0 || len(rest)%2 != 0 {
		return status.Errorf(codes.InvalidArgument,
			"invalid document name %q: it must name a document, not a collection", name)
	}
	for _, seg := range rest {
		if seg == "" {
			return status.Errorf(codes.InvalidArgument, "invalid document name %q", name)
		}
	}
	return nil
}

// Documents are stored as protojson, never encoding/json: a Value is a oneof,
// which lands in Go as an interface no plain JSON decoder can rebuild.
var (
	marshal   = protojson.MarshalOptions{}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// get reads one document, and reports whether it was there.
func (s *Service) get(ctx context.Context, name string) (*firestorepb.Document, uint64, bool, error) {
	raw, version, err := s.kv.Get(ctx, docKey(name))
	if err != nil {
		return nil, 0, false, nil
	}
	var doc firestorepb.Document
	if err := unmarshal.Unmarshal(raw, &doc); err != nil {
		return nil, 0, false, status.Errorf(codes.Internal, "decoding document: %v", err)
	}
	return &doc, version, true, nil
}
