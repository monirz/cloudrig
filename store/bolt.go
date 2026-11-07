package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bolt is a Store kept on disk, used when cloudrig runs with --data-dir.
//
// It replaced a Memory snapshotted to one JSON file, whose cost grew with the
// object count: every write rewrote the whole snapshot a second later, and the
// whole map stayed on the heap. Here a write is one small transaction, a List
// is a range scan, and the heap holds only what a call is using; the rest is
// the operating system's page cache.
type Bolt struct {
	db        *bolt.DB
	closeOnce sync.Once
	closeErr  error
}

var _ Store = (*Bolt)(nil)

// kvBucket holds every key. One bucket keeps List a single ordered scan.
var kvBucket = []byte("kv")

// OpenBolt opens the database at path, creating it if absent.
//
// Commits are not synced to disk one by one. Durability here is a convenience
// for a developer restarting the emulator, and a sync per write would cap it
// at a few hundred writes a second. Every commit reaches the operating system
// before the call returns, so a crash or kill of cloudrig loses nothing; only
// a power loss can. Close syncs.
func OpenBolt(path string) (*Bolt, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("store: preparing %s: %w", filepath.Dir(path), err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{
		// The file is locked while open. Without a timeout a second cloudrig
		// on the same --data-dir would hang instead of saying why.
		Timeout:        time.Second,
		NoSync:         true,
		NoFreelistSync: true,
		FreelistType:   bolt.FreelistMapType,
	})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, fmt.Errorf("store: %s is locked; is another cloudrig using this --data-dir?", path)
	}
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(kvBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: preparing %s: %w", path, err)
	}
	return &Bolt{db: db}, nil
}

// Close syncs and closes the database. Later calls return the first's result.
func (s *Bolt) Close() error {
	s.closeOnce.Do(func() {
		if err := s.db.Sync(); err != nil {
			_ = s.db.Close()
			s.closeErr = fmt.Errorf("store: syncing: %w", err)
			return
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// A stored value is its version, 8 bytes big-endian, then the caller's bytes.

func encodeEntry(version uint64, val []byte) []byte {
	out := make([]byte, 8+len(val))
	binary.BigEndian.PutUint64(out, version)
	copy(out[8:], val)
	return out
}

// decodeEntry copies the value out: bbolt's slices are only valid inside the
// transaction that read them.
func decodeEntry(raw []byte) ([]byte, uint64) {
	val := make([]byte, len(raw)-8)
	copy(val, raw[8:])
	return val, binary.BigEndian.Uint64(raw)
}

func entryVersion(raw []byte) uint64 { return binary.BigEndian.Uint64(raw) }

func (s *Bolt) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	var val []byte
	var version uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(kvBucket).Get([]byte(key))
		if raw == nil {
			return ErrNotFound
		}
		val, version = decodeEntry(raw)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return val, version, nil
}

// Put holds bbolt's single write transaction across the compare and the swap,
// which is what makes the CAS atomic.
func (s *Bolt) Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var next uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(kvBucket)
		raw := b.Get([]byte(key))
		var current uint64
		if raw != nil {
			current = entryVersion(raw)
		}
		switch {
		case ifVersion == 0 && raw != nil:
			return ErrVersionMismatch // required absent, found present
		case ifVersion != 0 && raw == nil:
			return ErrVersionMismatch // required a version, found nothing
		case ifVersion != 0 && current != ifVersion:
			return ErrVersionMismatch
		}
		// A fresh key lands on 1, keeping 0 reserved for "must not exist".
		next = current + 1
		return b.Put([]byte(key), encodeEntry(next, val))
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

func (s *Bolt) Delete(ctx context.Context, key string, ifVersion uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(kvBucket)
		raw := b.Get([]byte(key))
		if raw == nil {
			return ErrNotFound
		}
		if ifVersion != 0 && entryVersion(raw) != ifVersion {
			return ErrVersionMismatch
		}
		return b.Delete([]byte(key))
	})
}

func (s *Bolt) List(ctx context.Context, prefix string, limit int, pageToken string) ([]KV, string, error) {
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
		if len(decoded) < len(prefix) || decoded[:len(prefix)] != prefix {
			return nil, "", ErrInvalidPageToken
		}
		after = decoded
	}

	var out []KV
	var next string
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(kvBucket).Cursor()
		p := []byte(prefix)
		k, raw := c.Seek([]byte(after))
		// A page token names the last key returned, so skip past an exact match.
		if pageToken != "" && k != nil && string(k) == after {
			k, raw = c.Next()
		}
		out = make([]KV, 0, 16)
		for ; k != nil && bytes.HasPrefix(k, p); k, raw = c.Next() {
			if limit > 0 && len(out) == limit {
				next = encodeToken(out[len(out)-1].Key)
				return nil
			}
			val, version := decodeEntry(raw)
			out = append(out, KV{Key: string(k), Val: val, Version: version})
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

func (s *Bolt) Reset(ctx context.Context, keyPrefix string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if keyPrefix == "" {
			if err := tx.DeleteBucket(kvBucket); err != nil {
				return err
			}
			_, err := tx.CreateBucket(kvBucket)
			return err
		}
		// Seek again after each delete: a bbolt cursor that deletes under
		// itself can skip the next key.
		c := tx.Bucket(kvBucket).Cursor()
		p := []byte(keyPrefix)
		for k, _ := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, _ = c.Seek(p) {
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

// importBatch is how many entries one import transaction writes: large enough
// that an import of a million entries is quick, small enough that one
// transaction's dirty pages stay modest.
const importBatch = 20000

// ImportSnapshot loads a metadata.json written by an older cloudrig, whose
// store kept everything in memory and saved it whole to that one file.
//
// It runs only into an empty database, so it can never overwrite newer data,
// and afterwards renames the file with an ".imported" suffix: kept, in case it
// is wanted, but never imported twice. A missing file is not an error.
func (s *Bolt) ImportSnapshot(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: reading %s: %w", path, err)
	}

	var empty bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket(kvBucket).Cursor().First()
		empty = k == nil
		return nil
	}); err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("store: %s is from an older cloudrig, but the database beside it already has data; "+
			"move one of them away", path)
	}

	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		// Louder than starting empty: silently discarding a developer's data
		// because a file was unreadable is the worse failure.
		return fmt.Errorf("store: %s is not a cloudrig snapshot: %w", path, err)
	}
	for start := 0; start < len(entries); start += importBatch {
		batch := entries[start:min(start+importBatch, len(entries))]
		if err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(kvBucket)
			for _, e := range batch {
				if err := b.Put([]byte(e.Key), encodeEntry(e.Version, e.Val)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("store: importing %s: %w", path, err)
		}
	}
	if err := s.db.Sync(); err != nil {
		return fmt.Errorf("store: syncing the import: %w", err)
	}
	if err := os.Rename(path, path+".imported"); err != nil {
		return fmt.Errorf("store: setting aside %s: %w", path, err)
	}
	return nil
}
