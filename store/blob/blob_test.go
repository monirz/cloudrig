package blob_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/store/blob"
)

func newStore(t *testing.T) *blob.Store {
	t.Helper()
	s, err := blob.NewTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPutComputesTheChecksumsGCSReports(t *testing.T) {
	t.Parallel()

	content := []byte("hello, cloudrig")
	s := newStore(t)

	ref, err := s.Put(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	sha := sha256.Sum256(content)
	if ref.SHA256 != hex.EncodeToString(sha[:]) {
		t.Errorf("SHA256 = %q", ref.SHA256)
	}
	if ref.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", ref.Size, len(content))
	}

	sum := md5.Sum(content)
	if want := base64.StdEncoding.EncodeToString(sum[:]); ref.MD5 != want {
		t.Errorf("MD5 = %q, want %q", ref.MD5, want)
	}

	// Castagnoli, big-endian, base64 — not the IEEE polynomial hash/crc32
	// defaults to. A mismatch here is rejected by every real client.
	var crcBytes [4]byte
	binary.BigEndian.PutUint32(crcBytes[:], crc32.Checksum(content, crc32.MakeTable(crc32.Castagnoli)))
	if want := base64.StdEncoding.EncodeToString(crcBytes[:]); ref.CRC32C != want {
		t.Errorf("CRC32C = %q, want %q", ref.CRC32C, want)
	}
}

func TestCRC32CMatchesAKnownValue(t *testing.T) {
	t.Parallel()

	// Pinned rather than recomputed: a test that derives the expectation the
	// same way the code does would pass with the wrong polynomial.
	s := newStore(t)
	ref, err := s.Put(context.Background(), strings.NewReader("123456789"))
	if err != nil {
		t.Fatal(err)
	}
	// CRC-32C of "123456789" is 0xE3069283.
	if want := base64.StdEncoding.EncodeToString([]byte{0xE3, 0x06, 0x92, 0x83}); ref.CRC32C != want {
		t.Errorf("CRC32C = %q, want %q", ref.CRC32C, want)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":      "",
		"short":      "x",
		"multiline":  "a\nb\nc\n",
		"binary-ish": "\x00\x01\x02\xff",
	}

	s := newStore(t)
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			ref, err := s.Put(context.Background(), strings.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			f, err := s.Open(ref.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != content {
				t.Errorf("read %q, want %q", got, content)
			}
		})
	}
}

func TestIdenticalContentIsStoredOnce(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	first, err := s.Put(context.Background(), strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(context.Background(), strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("identical content produced different addresses")
	}
	// Content-addressed: the second Put is a hit, not a second file.
	if s.Path(first.SHA256) != s.Path(second.SHA256) {
		t.Error("identical content stored at two paths")
	}
}

func TestOpenMissing(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, err := s.Open(strings.Repeat("0", 64))
	if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ref, err := s.Put(context.Background(), strings.NewReader("gone soon"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ref.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ref.SHA256); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("still readable after delete: %v", err)
	}
	// Deleting what is not there is what the caller wanted anyway.
	if err := s.Delete(ref.SHA256); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestPathIsFannedOut(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	ref, err := s.Put(context.Background(), strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	// One directory holding every blob is slow to list on most filesystems.
	if !strings.Contains(s.Path(ref.SHA256), "/"+ref.SHA256[:2]+"/") {
		t.Errorf("Path = %q, want a two-character fan-out directory", s.Path(ref.SHA256))
	}
}

func TestPutLeavesNoStagingFiles(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.Put(context.Background(), strings.NewReader("content")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(s.Root() + "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d staging files left behind", len(entries))
	}
}

func TestPutPropagatesAReadFailure(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	_, err := s.Put(context.Background(), io.MultiReader(
		strings.NewReader("partial"),
		errReader{errors.New("connection reset")},
	))
	if err == nil {
		t.Fatal("a failed read produced a successful Put")
	}

	entries, _ := os.ReadDir(s.Root() + "/tmp")
	if len(entries) != 0 {
		t.Errorf("a failed Put left %d staging files behind", len(entries))
	}
}

func TestPutRespectsACancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newStore(t)
	if _, err := s.Put(ctx, strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
