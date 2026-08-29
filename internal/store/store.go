// Package store is cloudrig's metadata layer: a versioned, compare-and-swap
// key/value map plus a content-addressed blob tree. Object payload bytes never
// pass through it — those live in the blob tree and the metadata holds only a
// hash. See PLAN.md for the key layout.
package store

import (
	"context"
	"errors"
)

// Sentinel errors. Callers map these onto GCS status codes in core/gerr; the
// store itself knows nothing about HTTP.
var (
	// ErrNotFound is returned by Get and Delete for an absent key.
	ErrNotFound = errors.New("store: key not found")

	// ErrVersionMismatch is returned when a compare-and-swap loses: the key's
	// current version is not the one the caller required. This is what a GCS
	// precondition failure is built on, so it must be reported rather than
	// papered over with a retry.
	ErrVersionMismatch = errors.New("store: version mismatch")

	// ErrInvalidPageToken is returned by List for a token it did not issue.
	ErrInvalidPageToken = errors.New("store: invalid page token")
)

// KV is one entry. List returns keys alongside values because callers need the
// key itself — GCS delimiter rollup synthesizes prefixes[] from key structure,
// and recovering a name by decoding the value it labels would be absurd.
type KV struct {
	Key     string
	Val     []byte
	Version uint64
}

// Store is a versioned key/value map with compare-and-swap writes.
//
// Versions start at 1 and increase on every write to a key, so version 0 is
// never a live value and is free to carry a distinct meaning in the
// precondition arguments below.
type Store interface {
	// Get returns the value and its current version, or ErrNotFound.
	Get(ctx context.Context, key string) (val []byte, version uint64, err error)

	// Put writes val and returns the new version.
	//
	// ifVersion is a precondition: 0 requires that the key not exist, and any
	// other value requires that it be the key's current version. There is no
	// unconditional Put and no read-then-write, because GCS preconditions
	// depend on the compare and the swap being one atomic step.
	//
	// A failed precondition is ErrVersionMismatch, whether the key was absent
	// when it had to exist or present when it had to be absent.
	Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error)

	// Delete removes a key. ifVersion of 0 deletes unconditionally; any other
	// value requires it to be the current version. Deleting an absent key is
	// ErrNotFound.
	Delete(ctx context.Context, key string, ifVersion uint64) error

	// List returns entries whose key has the given prefix, in lexicographic key
	// order, starting after pageToken.
	//
	// limit is a scan budget, not a page size. A caller collapsing many keys
	// into one delimiter prefix may consume far more entries than it emits, so
	// pagination visible to the API is built above this, not here. A limit of 0
	// or less means no budget.
	//
	// The returned token is empty when the scan reached the end of the prefix.
	List(ctx context.Context, prefix string, limit int, pageToken string) ([]KV, string, error)

	// Reset removes every key under keyPrefix. An empty prefix removes
	// everything.
	Reset(ctx context.Context, keyPrefix string) error
}
