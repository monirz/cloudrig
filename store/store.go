// Package store is cloudrig's metadata layer: a versioned compare-and-swap
// key/value map. Payload bytes never pass through it, only blob hashes.
package store

import (
	"context"
	"errors"
)

// Sentinel errors. Callers map these onto status codes; the store has no HTTP.
var (
	// ErrNotFound is returned by Get and Delete for an absent key.
	ErrNotFound = errors.New("store: key not found")

	// ErrVersionMismatch means a compare-and-swap lost. GCS preconditions are
	// built on it, so it must be reported, never retried away.
	ErrVersionMismatch = errors.New("store: version mismatch")

	// ErrInvalidPageToken is returned by List for a token it did not issue.
	ErrInvalidPageToken = errors.New("store: invalid page token")
)

// PageToken builds a token that resumes a List after key. Callers that stop
// part-way through a page need to name where they stopped, and the token format
// belongs to the store rather than to them.
func PageToken(key string) string { return encodeToken(key) }

// KV is one entry. List returns keys with values because GCS delimiter rollup
// synthesizes prefixes[] from key structure.
type KV struct {
	Key     string
	Val     []byte
	Version uint64
}

// Store is a versioned key/value map with compare-and-swap writes. Versions
// start at 1, leaving 0 free to mean something else in the preconditions.
type Store interface {
	// Get returns the value and its current version, or ErrNotFound.
	Get(ctx context.Context, key string) (val []byte, version uint64, err error)

	// Put writes val and returns the new version. ifVersion of 0 requires the
	// key be absent; anything else requires it be the current version.
	// There is no unconditional Put: the compare and swap must be one step.
	Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error)

	// Delete removes a key. ifVersion of 0 deletes unconditionally; anything
	// else requires it be the current version. An absent key is ErrNotFound.
	Delete(ctx context.Context, key string, ifVersion uint64) error

	// List returns entries under prefix in key order, starting after pageToken.
	// limit is a scan budget, not a page size: delimiter rollup may consume far
	// more entries than it emits, so API pagination is built above this.
	List(ctx context.Context, prefix string, limit int, pageToken string) ([]KV, string, error)

	// Reset removes every key under keyPrefix; an empty prefix removes all.
	Reset(ctx context.Context, keyPrefix string) error
}
