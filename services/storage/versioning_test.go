package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/services/storage"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

// serviceWithBlobs returns a service and the blob tree behind it, so a test can
// count files on disk rather than trust the metadata.
func serviceWithBlobs(t *testing.T, versioning bool) (*storage.Service, *blob.Store, context.Context) {
	t.Helper()

	blobs, err := blob.NewTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	s := storage.New(store.NewMemory(), blobs, clock.NewFake(epoch), nil)
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, storage.Bucket{
		Name: "bkt", Project: "p", Versioning: versioning,
	}); err != nil {
		t.Fatal(err)
	}
	return s, blobs, ctx
}

func blobCount(t *testing.T, blobs *blob.Store) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(filepath.Join(blobs.Root(), "blobs"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func put(t *testing.T, s *storage.Service, ctx context.Context, name, content string) storage.Object {
	t.Helper()
	obj, err := s.WriteObject(ctx, "p", storage.Write{Bucket: "bkt", Name: name}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// TestUnversionedReclaimsContent is the fix for unbounded growth: without
// versioning, overwriting or deleting an object must free what it held.
func TestUnversionedReclaimsContent(t *testing.T) {
	t.Parallel()
	s, blobs, ctx := serviceWithBlobs(t, false)

	const overwrites = 20
	for i := 0; i < overwrites; i++ {
		put(t, s, ctx, "obj", strings.Repeat("x", i+1))
	}
	if got := blobCount(t, blobs); got != 1 {
		t.Errorf("%d blobs after %d overwrites, want 1", got, overwrites)
	}

	if err := s.DeleteObject(ctx, "p", "bkt", "obj", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if got := blobCount(t, blobs); got != 0 {
		t.Errorf("%d blobs after deleting the object, want 0", got)
	}
}

// TestVersionedKeepsGenerations is the other half: with versioning on, both the
// metadata and the content stay, as GCS does.
func TestVersionedKeepsGenerations(t *testing.T) {
	t.Parallel()
	s, blobs, ctx := serviceWithBlobs(t, true)

	first := put(t, s, ctx, "obj", "one")
	second := put(t, s, ctx, "obj", "two")

	if got := blobCount(t, blobs); got != 2 {
		t.Errorf("%d blobs after an overwrite, want 2", got)
	}
	if _, err := s.GetObject(ctx, "p", "bkt", "obj", &first.Generation); err != nil {
		t.Errorf("the archived generation is unreadable: %v", err)
	}

	if err := s.DeleteObject(ctx, "p", "bkt", "obj", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetObject(ctx, "p", "bkt", "obj", &second.Generation); err != nil {
		t.Errorf("the deleted generation is unreadable in a versioned bucket: %v", err)
	}
	if got := blobCount(t, blobs); got != 2 {
		t.Errorf("%d blobs after a delete in a versioned bucket, want 2", got)
	}
}

// TestSharedContentSurvivesOneDelete is why the reclaim is refcounted rather
// than a plain remove: identical bytes are one file, and deleting one object
// must not take the other's content with it.
func TestSharedContentSurvivesOneDelete(t *testing.T) {
	t.Parallel()
	s, blobs, ctx := serviceWithBlobs(t, false)

	put(t, s, ctx, "a", "identical")
	put(t, s, ctx, "b", "identical")

	// Content-addressed: two objects, one file.
	if got := blobCount(t, blobs); got != 1 {
		t.Fatalf("%d blobs for two identical objects, want 1", got)
	}

	if err := s.DeleteObject(ctx, "p", "bkt", "a", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if got := blobCount(t, blobs); got != 1 {
		t.Errorf("%d blobs after deleting one of two sharers, want 1", got)
	}

	// The survivor must still read, not just exist.
	_, f, err := s.OpenObject(ctx, "p", "bkt", "b", nil, storage.Preconditions{})
	if err != nil {
		t.Fatalf("the surviving object lost its content: %v", err)
	}
	f.Close()

	if err := s.DeleteObject(ctx, "p", "bkt", "b", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if got := blobCount(t, blobs); got != 0 {
		t.Errorf("%d blobs after deleting the last sharer, want 0", got)
	}
}

// TestRewritingIdenticalContentKeepsIt guards an off-by-one in the refcount:
// overwriting an object with the same bytes releases the old generation and
// retains the new one, both naming the same file.
func TestRewritingIdenticalContentKeepsIt(t *testing.T) {
	t.Parallel()
	s, blobs, ctx := serviceWithBlobs(t, false)

	for i := 0; i < 5; i++ {
		put(t, s, ctx, "obj", "unchanging")
	}
	if got := blobCount(t, blobs); got != 1 {
		t.Fatalf("%d blobs, want 1", got)
	}

	_, f, err := s.OpenObject(ctx, "p", "bkt", "obj", nil, storage.Preconditions{})
	if err != nil {
		t.Fatalf("the object lost its content to its own rewrite: %v", err)
	}
	f.Close()
}

// TestConcurrentWritesDoNotLoseContent runs the refcount under contention: the
// winner's content must survive every loser's release.
func TestConcurrentWritesDoNotLoseContent(t *testing.T) {
	t.Parallel()
	s, _, ctx := serviceWithBlobs(t, false)

	const racers = 16
	done := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = s.WriteObject(ctx, "p", storage.Write{Bucket: "bkt", Name: "obj"},
				strings.NewReader("same content for everyone"))
		}()
	}
	for i := 0; i < racers; i++ {
		<-done
	}

	_, f, err := s.OpenObject(ctx, "p", "bkt", "obj", nil, storage.Preconditions{})
	if err != nil {
		t.Fatalf("the surviving object has no content: %v", err)
	}
	f.Close()
}
