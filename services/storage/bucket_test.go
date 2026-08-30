package storage_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/services/storage"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newService(t *testing.T) (*storage.Service, *clock.FakeClock) {
	t.Helper()

	blobs, err := blob.NewTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	clk := clock.NewFake(epoch)
	return storage.New(store.NewMemory(), blobs, clk, nil), clk
}

// status is the HTTP status a semantics error renders as. Asserting on it here
// keeps the status contract with the layer that decides it, rather than only in
// the handler tests.
func status(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	var g *gerr.Error
	if !errors.As(err, &g) {
		t.Fatalf("error %v is not a gerr.Error", err)
	}
	return g.HTTPStatus()
}

func TestCreateBucket(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()

	b, err := s.CreateBucket(ctx, storage.Bucket{Name: "my-bucket", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}

	if b.Location != storage.DefaultLocation || b.StorageClass != storage.DefaultStorageClass {
		t.Errorf("defaults not applied: %+v", b)
	}
	if b.Metageneration != 1 {
		t.Errorf("Metageneration = %d, want 1", b.Metageneration)
	}
	// Timestamps come from the injected clock, so they are the fake epoch.
	if !b.Created.Equal(epoch) || !b.Updated.Equal(epoch) {
		t.Errorf("timestamps = %v/%v, want the fake epoch", b.Created, b.Updated)
	}
}

func TestCreateBucketRejectsDuplicates(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "taken", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateBucket(ctx, storage.Bucket{Name: "taken", Project: "p"})
	if got := status(t, err); got != 409 {
		t.Errorf("status = %d, want 409", got)
	}

	// The namespace is global, as in GCS: a name taken in one project is taken
	// everywhere. Per-test isolation comes from each MustStart being its own
	// emulator, not from partitioning names by project.
	_, err = s.CreateBucket(ctx, storage.Bucket{Name: "taken", Project: "other"})
	if got := status(t, err); got != 409 {
		t.Errorf("status = %d for the same name in another project, want 409", got)
	}
}

// TestConcurrentCreateHasOneWinner is why create uses a compare-and-swap rather
// than read-then-write.
func TestConcurrentCreateHasOneWinner(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)

	const racers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.CreateBucket(context.Background(),
				storage.Bucket{Name: "contested", Project: "p"}); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Errorf("%d creates succeeded, want 1", won)
	}
}

func TestCreateBucketValidatesTheName(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)

	tests := map[string]string{
		"empty":       "",
		"too short":   "ab",
		"too long":    strings.Repeat("a", 223),
		"has a slash": "a/b",
		"has a NUL":   "a\x00b",
	}

	for name, bucket := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := s.CreateBucket(context.Background(), storage.Bucket{Name: bucket, Project: "p"})
			if got := status(t, err); got != 400 {
				t.Errorf("status = %d, want 400", got)
			}
		})
	}

	t.Run("no project", func(t *testing.T) {
		_, err := s.CreateBucket(context.Background(), storage.Bucket{Name: "valid-name"})
		if got := status(t, err); got != 400 {
			t.Errorf("status = %d, want 400", got)
		}
	})
}

func TestGetBucket(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p", Location: "EU"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetBucket(ctx, "p", "bkt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != "EU" {
		t.Errorf("Location = %q, want EU", got.Location)
	}

	if _, err := s.GetBucket(ctx, "p", "missing"); status(t, err) != 404 {
		t.Errorf("status = %d, want 404", status(t, err))
	}
	// A bucket in another project is not visible.
	if _, err := s.GetBucket(ctx, "other", "bkt"); status(t, err) != 404 {
		t.Error("a bucket leaked across projects")
	}
}

func TestListBuckets(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()

	for _, name := range []string{"beta", "alpha"} {
		if _, err := s.CreateBucket(ctx, storage.Bucket{Name: name, Project: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "elsewhere", Project: "other"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ProjectOf(ctx, "elsewhere"); err != nil || got != "other" {
		t.Errorf("ProjectOf = %q, %v; want other", got, err)
	}

	got, err := s.ListBuckets(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("buckets = %+v, want alpha then beta", got)
	}

	empty, err := s.ListBuckets(ctx, "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("a project with no buckets returned %d", len(empty))
	}
}

func TestDeleteBucket(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBucket(ctx, "p", "bkt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBucket(ctx, "p", "bkt"); status(t, err) != 404 {
		t.Error("bucket survived deletion")
	}
	if err := s.DeleteBucket(ctx, "p", "bkt"); status(t, err) != 404 {
		t.Errorf("status = %d deleting a missing bucket, want 404", status(t, err))
	}
}

func TestReset(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "other-bkt", Project: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := write(t, s, "obj", "content", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}

	t.Run("scoped to a project", func(t *testing.T) {
		if err := s.Reset(ctx, "p"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetBucket(ctx, "p", "bkt"); status(t, err) != 404 {
			t.Error("the project's bucket survived its reset")
		}
		// Another project is untouched.
		if _, err := s.GetBucket(ctx, "other", "other-bkt"); err != nil {
			t.Errorf("another project's bucket was cleared: %v", err)
		}
		// The name claim was released, so the name is reusable.
		if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
			t.Errorf("the bucket name was not released: %v", err)
		}
	})

	t.Run("everything", func(t *testing.T) {
		if err := s.Reset(ctx, ""); err != nil {
			t.Fatal(err)
		}
		for _, b := range []struct{ project, name string }{{"p", "bkt"}, {"other", "other-bkt"}} {
			if _, err := s.GetBucket(ctx, b.project, b.name); status(t, err) != 404 {
				t.Errorf("%s/%s survived a full reset", b.project, b.name)
			}
		}
		if got, err := s.ListBuckets(ctx, "p"); err != nil || len(got) != 0 {
			t.Errorf("ListBuckets = %v, %v after a full reset", got, err)
		}
	})
}
