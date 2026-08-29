package store

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"sync"
)

// Memory is an in-process Store. Every mutation takes the write lock, which
// makes compare-and-swap trivially atomic: the compare and the swap happen
// without releasing it, so two writers racing the same precondition cannot both
// observe the version they need.
type Memory struct {
	mu sync.RWMutex
	m  map[string]entry

	// keys is the sorted key set, maintained on write so List does not sort the
	// whole map on every call. Listing is the hot path for object enumeration;
	// writes are comparatively rare.
	keys []string
}

type entry struct {
	val     []byte
	version uint64
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory { return &Memory{m: make(map[string]entry)} }

var _ Store = (*Memory)(nil)

func (s *Memory) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.m[key]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return clone(e.val), e.version, nil
}

func (s *Memory) Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.m[key]
	switch {
	case ifVersion == 0 && ok:
		return 0, ErrVersionMismatch // required absent, found present
	case ifVersion != 0 && !ok:
		return 0, ErrVersionMismatch // required a version, found nothing
	case ifVersion != 0 && e.version != ifVersion:
		return 0, ErrVersionMismatch
	}

	if !ok {
		s.insertKey(key)
	}
	// Versions start at 1, so a fresh key lands on 1 and 0 stays reserved for
	// "must not exist" in the precondition arguments.
	next := e.version + 1
	s.m[key] = entry{val: clone(val), version: next}
	return next, nil
}

func (s *Memory) Delete(ctx context.Context, key string, ifVersion uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.m[key]
	if !ok {
		return ErrNotFound
	}
	if ifVersion != 0 && e.version != ifVersion {
		return ErrVersionMismatch
	}
	delete(s.m, key)
	s.removeKey(key)
	return nil
}

func (s *Memory) List(ctx context.Context, prefix string, limit int, pageToken string) ([]KV, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	after := prefix
	if pageToken != "" {
		decoded, err := decodeToken(pageToken)
		if err != nil {
			return nil, "", err
		}
		// A token from a different prefix would silently return the wrong page.
		if !strings.HasPrefix(decoded, prefix) {
			return nil, "", ErrInvalidPageToken
		}
		after = decoded
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// sort.SearchStrings finds the first key >= after; a page token names the
	// last key already returned, so skip past an exact match.
	i := sort.SearchStrings(s.keys, after)
	if pageToken != "" && i < len(s.keys) && s.keys[i] == after {
		i++
	}

	out := make([]KV, 0, 16)
	for ; i < len(s.keys); i++ {
		k := s.keys[i]
		if !strings.HasPrefix(k, prefix) {
			break // sorted, so the prefix range has ended
		}
		if limit > 0 && len(out) == limit {
			return out, encodeToken(out[len(out)-1].Key), nil
		}
		e := s.m[k]
		out = append(out, KV{Key: k, Val: clone(e.val), Version: e.version})
	}
	return out, "", nil
}

func (s *Memory) Reset(ctx context.Context, keyPrefix string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if keyPrefix == "" {
		s.m = make(map[string]entry)
		s.keys = nil
		return nil
	}

	kept := s.keys[:0]
	for _, k := range s.keys {
		if strings.HasPrefix(k, keyPrefix) {
			delete(s.m, k)
			continue
		}
		kept = append(kept, k)
	}
	s.keys = kept
	return nil
}

// insertKey and removeKey keep s.keys sorted. Callers hold the write lock.

func (s *Memory) insertKey(key string) {
	i := sort.SearchStrings(s.keys, key)
	s.keys = append(s.keys, "")
	copy(s.keys[i+1:], s.keys[i:])
	s.keys[i] = key
}

func (s *Memory) removeKey(key string) {
	i := sort.SearchStrings(s.keys, key)
	if i < len(s.keys) && s.keys[i] == key {
		s.keys = append(s.keys[:i], s.keys[i+1:]...)
	}
}

// clone defends the map against callers mutating a slice they handed in or got
// back. Metadata values are small; payload bytes never reach this package.
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// Page tokens are opaque by contract, so they are encoded rather than handed
// out as raw keys — a caller that parses one is relying on something we do not
// promise.
func encodeToken(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeToken(tok string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", ErrInvalidPageToken
	}
	return string(b), nil
}
