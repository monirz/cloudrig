package firestore

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const base = "projects/p/databases/(default)/documents"

func TestValidDocName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"a document", base + "/people/ada", true},
		{"a subcollection document", base + "/people/ada/pets/cat", true},
		{"a collection is not a document", base + "/people", false},
		{"a deeper collection", base + "/people/ada/pets", false},
		{"no documents marker", "projects/p/people/ada", false},
		{"an empty segment", base + "/people//ada", false},
		{"nothing at all", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validDocName(c.in)
			if c.ok && err != nil {
				t.Errorf("validDocName(%q) = %v, want ok", c.in, err)
			}
			if !c.ok && status.Code(err) != codes.InvalidArgument {
				t.Errorf("validDocName(%q) = %v, want InvalidArgument", c.in, err)
			}
		})
	}
}

// TestKeysAreNamespaced keeps documents from colliding with any other
// service's keys in the shared store.
func TestKeysAreNamespaced(t *testing.T) {
	t.Parallel()

	if got := docKey(base + "/people/ada"); got != "fs/d/"+base+"/people/ada" {
		t.Errorf("docKey = %q", got)
	}
	// A collection's prefix must not also match a document whose name merely
	// starts with the same letters.
	if got := collectionPrefix(base, "people"); got != "fs/d/"+base+"/people/" {
		t.Errorf("collectionPrefix = %q", got)
	}
}

// TestTransactionsAreBounded covers the two limits on open handles: a client
// that never commits or rolls back must not grow the map without end.
func TestTransactionsAreBounded(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := New(store.NewMemory(), clk)
	ctx := context.Background()

	for i := 0; i < MaxOpenTransactions; i++ {
		if _, err := s.BeginTransaction(ctx, &firestorepb.BeginTransactionRequest{}); err != nil {
			t.Fatalf("transaction %d: %v", i, err)
		}
	}
	_, err := s.BeginTransaction(ctx, &firestorepb.BeginTransactionRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("err = %v, want ResourceExhausted past the cap", err)
	}

	// Abandoned handles expire, so the cap is a limit on live work rather
	// than on how long the emulator has been running.
	clk.Advance(TransactionTTL + time.Second)
	if _, err := s.BeginTransaction(ctx, &firestorepb.BeginTransactionRequest{}); err != nil {
		t.Errorf("no transaction could be opened after the old ones expired: %v", err)
	}

	s.mu.Lock()
	live := len(s.transactions)
	s.mu.Unlock()
	if live != 1 {
		t.Errorf("%d handles are still held, want only the new one", live)
	}
}

// TestUnknownTransactionIsRejected keeps a made-up handle from committing.
func TestUnknownTransactionIsRejected(t *testing.T) {
	t.Parallel()

	s := New(store.NewMemory(), clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	_, err := s.Commit(context.Background(), &firestorepb.CommitRequest{
		Transaction: []byte("invented"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}
