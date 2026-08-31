package firestore

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// BeginTransaction opens a transaction and returns its handle.
//
// Isolation here is the commit lock, not multi-version concurrency: a commit
// applies as a unit and one commit at a time. That is enough to make
// RunTransaction's read-then-write body behave, and short of real Firestore,
// which aborts a transaction whose reads were invalidated. A test that depends
// on seeing that abort will not see it here.
func (s *Service) BeginTransaction(ctx context.Context, req *firestorepb.BeginTransactionRequest) (*firestorepb.BeginTransactionResponse, error) {
	id, err := newTransactionID()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.transactions[string(id)] = struct{}{}

	return &firestorepb.BeginTransactionResponse{Transaction: id}, nil
}

// Rollback discards a transaction. Nothing has been written, so this only
// forgets the handle.
func (s *Service) Rollback(ctx context.Context, req *firestorepb.RollbackRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.transactions[string(req.GetTransaction())]; !ok {
		return nil, status.Error(codes.InvalidArgument, "unknown transaction")
	}
	delete(s.transactions, string(req.GetTransaction()))
	return &emptypb.Empty{}, nil
}

// closeTransaction retires a handle at commit. The caller holds the lock.
func (s *Service) closeTransaction(id []byte) error {
	if len(id) == 0 {
		return nil
	}
	if _, ok := s.transactions[string(id)]; !ok {
		return status.Error(codes.InvalidArgument, "unknown transaction")
	}
	delete(s.transactions, string(id))
	return nil
}

func newTransactionID() ([]byte, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, status.Errorf(codes.Internal, "opening a transaction: %v", err)
	}
	// Printable, so a handle in a log or an error is readable.
	out := make([]byte, base64.RawURLEncoding.EncodedLen(len(buf)))
	base64.RawURLEncoding.Encode(out, buf)
	return out, nil
}
