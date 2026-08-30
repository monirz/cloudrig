package store

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"sync"
)

// Memory is an in-process Store. Every mutation holds the write lock across
// both the compare and the swap, which is what makes the CAS atomic.
type Memory struct {
	mu sync.RWMutex
	m  map[string]entry

	// keys is sorted on write so List need not sort the map on every call.
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
	// A fresh key lands on 1, keeping 0 reserved for "must not exist".
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

	// A page token names the last key returned, so skip past an exact match.
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

// Entry is one key's stored state, for a snapshot. Version travels with it:
// restoring without it would reset every compare-and-swap baseline, so a
// precondition that held before a restart would fail after one.
type Entry struct {
	Key     string `json:"key"`
	Val     []byte `json:"val"`
	Version uint64 `json:"version"`
}

// Snapshot returns every entry, in key order.
func (s *Memory) Snapshot() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, 0, len(s.keys))
	for _, k := range s.keys {
		e := s.m[k]
		out = append(out, Entry{Key: k, Val: clone(e.val), Version: e.version})
	}
	return out
}

// Restore replaces the contents with entries.
func (s *Memory) Restore(entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.m = make(map[string]entry, len(entries))
	s.keys = make([]string, 0, len(entries))
	for _, e := range entries {
		s.m[e.Key] = entry{val: clone(e.Val), version: e.Version}
		s.keys = append(s.keys, e.Key)
	}
	sort.Strings(s.keys)
}

// clone stops callers mutating a slice they handed in or got back.
func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// Page tokens are opaque by contract, so they are encoded rather than raw
// keys: a caller that parses one relies on something we do not promise.
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
