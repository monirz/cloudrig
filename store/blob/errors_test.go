package blob_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/store/blob"
)

// badName holds a NUL, which no filesystem accepts: it makes an open or a
// remove fail with something other than "not found".
const badName = "ab" + "\x00" + "cdef"

func TestNewRefusesARootThatIsAFile(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := blob.New(file); err == nil {
		t.Error("a regular file was accepted as a blob root")
	}
}

// Opening and deleting distinguish "not there" from "could not be read": only
// the first is a normal outcome.
func TestOpenAndDeleteReportRealFailures(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	_, err := s.Open(badName)
	if err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Open = %v, want a failure that is not ErrNotFound", err)
	}
	if err := s.Delete(badName); err == nil {
		t.Error("Delete swallowed a failure that was not a missing file")
	}
}

// Deleting content that was never there is what the caller wanted.
func TestDeleteMissingIsNotAnError(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	if err := s.Delete(strings.Repeat("0", 64)); err != nil {
		t.Errorf("Delete of absent content = %v", err)
	}
}

// A digest too short to fan out still has somewhere to live.
func TestPathHandlesAShortDigest(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	if got := s.Path("a"); !strings.Contains(got, "__") {
		t.Errorf("Path(%q) = %q, want the undersized bucket", "a", got)
	}
	if got := s.Path(""); !strings.Contains(got, "__") {
		t.Errorf("Path(\"\") = %q, want the undersized bucket", got)
	}
}

// The destination directory cannot be made, because a file already sits where
// it would go.
func TestPutReportsAnUnusableDestination(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	const content = "hello"
	sum := sha256.Sum256([]byte(content))
	fan := hex.EncodeToString(sum[:])[:2]

	blobs := filepath.Join(s.Root(), "blobs")
	if err := os.MkdirAll(blobs, 0o750); err != nil {
		t.Fatal(err)
	}
	// Where the fan-out directory belongs, put a file instead.
	if err := os.WriteFile(filepath.Join(blobs, fan), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Put(context.Background(), strings.NewReader(content)); err == nil {
		t.Error("Put succeeded with no usable destination")
	}
}

// A tree that has never held content is empty, not broken.
func TestForkAndEachOnAnEmptyTree(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	fork, err := s.Fork()
	if err != nil {
		t.Fatalf("Fork of an empty tree: %v", err)
	}
	t.Cleanup(func() { _ = fork.Close() })

	if err := s.Each(func(int64, io.Reader) error {
		t.Error("an empty tree yielded a blob")
		return nil
	}); err != nil {
		t.Errorf("Each over an empty tree = %v", err)
	}
}

func TestEachVisitsEveryBlob(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	want := map[string]bool{"one": false, "two": false, "three": false}
	for content := range want {
		if _, err := s.Put(ctx, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}

	var seen int
	err := s.Each(func(size int64, r io.Reader) error {
		body, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if int64(len(body)) != size {
			t.Errorf("size = %d, want %d", size, len(body))
		}
		if _, ok := want[string(body)]; !ok {
			t.Errorf("unexpected blob %q", body)
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Errorf("visited %d blobs, want %d", seen, len(want))
	}
}

// An error from the callback is the caller's, and stops the walk.
func TestEachPropagatesTheCallbackError(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	if _, err := s.Put(context.Background(), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("enough")
	if err := s.Each(func(int64, io.Reader) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Each = %v, want the callback's error", err)
	}
}

// Reset empties the tree and leaves it usable, which is what a test harness
// between cases needs.
func TestResetLeavesTheTreeUsable(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	ref, err := s.Put(ctx, strings.NewReader("before"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ref.SHA256); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("content survived Reset: %v", err)
	}
	if _, err := s.Put(ctx, strings.NewReader("after")); err != nil {
		t.Errorf("the tree was not usable after Reset: %v", err)
	}
}

// A store handed a directory it did not create leaves that directory alone.
func TestCloseKeepsADirectoryItDoesNotOwn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Close removed a directory the store did not own: %v", err)
	}
}

// A fork shares the bytes but not the right to delete them.
func TestForkKeepsItsOwnCopyReadable(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	ref, err := s.Put(ctx, strings.NewReader("shared"))
	if err != nil {
		t.Fatal(err)
	}
	fork, err := s.Fork()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fork.Close() })

	if err := s.Delete(ref.SHA256); err != nil {
		t.Fatal(err)
	}
	f, err := fork.Open(ref.SHA256)
	if err != nil {
		t.Fatalf("the fork lost content the parent deleted: %v", err)
	}
	defer f.Close()

	body, err := io.ReadAll(f)
	if err != nil || string(body) != "shared" {
		t.Errorf("fork content = %q, %v", body, err)
	}
}
