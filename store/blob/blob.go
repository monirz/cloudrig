// Package blob stores object payloads as content-addressed files.
//
// Bytes never sit in the heap in full: an upload streams from the request body
// through the hashers to a temp file in one pass, and a download is served from
// the file. Memory stays flat whatever the object's size — the property
// fake-gcs-server does not have, where a 2 GB file has driven RSS past 12 GB.
package blob

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/monirz/cloudrig/core/tmp"
)

// ErrNotFound is returned for a digest the store does not hold.
var ErrNotFound = errors.New("blob: not found")

// castagnoli is the CRC-32C polynomial GCS uses. Not the IEEE one that
// hash/crc32 defaults to — a mismatch here produces a checksum every real
// client rejects.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Ref describes stored content: its address, and the checksums GCS reports.
type Ref struct {
	// SHA256 is the content address, lowercase hex.
	SHA256 string

	Size int64

	// CRC32C is base64 of the four big-endian checksum bytes, as GCS spells it.
	CRC32C string

	// MD5 is base64 of the digest, as GCS spells it.
	MD5 string
}

// Store is a content-addressed blob tree rooted at a directory.
type Store struct {
	root  string
	owned bool // true when the store created its own directory and may remove it
}

// New opens a blob tree under root, creating it if needed.
func New(root string) (*Store, error) {
	s := &Store{root: root}
	if err := os.MkdirAll(s.tmpDir(), 0o750); err != nil {
		return nil, fmt.Errorf("blob: preparing %s: %w", root, err)
	}
	return s, nil
}

// NewTemp opens a blob tree in a temporary directory, which Close removes.
func NewTemp() (*Store, error) {
	dir, err := tmp.Dir("blobs")
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	s, err := New(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.owned = true
	return s, nil
}

// Put streams r into the tree and returns its Ref.
//
// One pass: the reader is copied into the temp file and all three hashes at
// once, so the bytes are never read twice and never held whole.
func (s *Store) Put(ctx context.Context, r io.Reader) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}

	tmp, err := os.CreateTemp(s.tmpDir(), "put-")
	if err != nil {
		return Ref{}, fmt.Errorf("blob: opening a staging file: %w", err)
	}
	staged := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(staged) // a no-op once the file has been renamed away
	}()

	h := newHashes()
	size, err := io.Copy(io.MultiWriter(tmp, h.sha, h.crc, h.md5), r)
	if err != nil {
		return Ref{}, fmt.Errorf("blob: streaming content: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Ref{}, fmt.Errorf("blob: closing the staging file: %w", err)
	}

	ref := h.ref(size)
	dest := s.Path(ref.SHA256)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return Ref{}, fmt.Errorf("blob: preparing the destination: %w", err)
	}

	// Identical content is already stored under the same name, so an existing
	// destination is a hit rather than a conflict: keep it and drop the copy.
	if _, err := os.Stat(dest); err == nil {
		return ref, nil
	}
	if err := os.Rename(staged, dest); err != nil {
		return Ref{}, fmt.Errorf("blob: storing content: %w", err)
	}
	return ref, nil
}

// Open returns the content for a digest. The caller closes it.
func (s *Store) Open(sha256hex string) (*os.File, error) {
	f, err := os.Open(s.Path(sha256hex))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, sha256hex)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	return f, nil
}

// Path is where a digest's content lives. Exported so a handler can hand the
// file to http.ServeContent, which streams and handles Range itself.
func (s *Store) Path(sha256hex string) string {
	if len(sha256hex) < 2 {
		return filepath.Join(s.root, "blobs", "__", sha256hex)
	}
	// Fanned out by the first byte: one directory holding every blob makes
	// listing and lookup slow on most filesystems.
	return filepath.Join(s.root, "blobs", sha256hex[:2], sha256hex)
}

// Delete removes content. Deleting what is not there is not an error: the
// caller wanted it gone.
func (s *Store) Delete(sha256hex string) error {
	err := os.Remove(s.Path(sha256hex))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: %w", err)
	}
	return nil
}

// Reset removes every blob, leaving the tree usable.
func (s *Store) Reset() error {
	if err := os.RemoveAll(filepath.Join(s.root, "blobs")); err != nil {
		return fmt.Errorf("blob: clearing content: %w", err)
	}
	if err := os.MkdirAll(s.tmpDir(), 0o750); err != nil {
		return fmt.Errorf("blob: %w", err)
	}
	return nil
}

// Close removes the tree if the store created it.
func (s *Store) Close() error {
	if !s.owned {
		return nil
	}
	return os.RemoveAll(s.root)
}

// Root is the directory the tree lives under.
func (s *Store) Root() string { return s.root }

func (s *Store) tmpDir() string { return filepath.Join(s.root, "tmp") }

// hashes are the three digests computed in the single pass.
type hashes struct {
	sha hash.Hash
	crc hash.Hash32
	md5 hash.Hash
}

func newHashes() hashes {
	return hashes{
		sha: sha256.New(),
		crc: crc32.New(castagnoli),
		md5: md5.New(),
	}
}

func (h hashes) ref(size int64) Ref {
	var crcBytes [4]byte
	// Big-endian, then base64: the encoding GCS uses for crc32c.
	binary.BigEndian.PutUint32(crcBytes[:], h.crc.Sum32())

	return Ref{
		SHA256: hex.EncodeToString(h.sha.Sum(nil)),
		Size:   size,
		CRC32C: base64.StdEncoding.EncodeToString(crcBytes[:]),
		MD5:    base64.StdEncoding.EncodeToString(h.md5.Sum(nil)),
	}
}
