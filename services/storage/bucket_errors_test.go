package storage_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/services/storage"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

const (
	indexKey  = "bx/"
	bucketKey = "p/"
)

// newFaulty is withFaultyBucket without the bucket: some paths have to be
// armed before anything exists.
func newFaulty(t *testing.T) (*storage.Service, *faultyStore, context.Context) {
	t.Helper()

	blobs, err := blob.NewTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	fs := &faultyStore{Store: store.NewMemory()}
	return storage.New(fs, blobs, clock.NewFake(epoch), nil), fs, context.Background()
}

func makeBucket(t *testing.T, s *storage.Service, ctx context.Context, name string) {
	t.Helper()
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: name, Project: "p"}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBucketRejectsBadNames(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()

	tests := []struct{ name, bucket string }{
		{"empty", ""},
		{"too short", "ab"},
		{"too long", strings.Repeat("a", 223)},
		{"a slash", "has/slash"},
		{"a NUL", "has\x00nul"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.CreateBucket(ctx, storage.Bucket{Name: tc.bucket, Project: "p"})
			if got := status(t, err); got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (err: %v)", got, err)
			}
		})
	}
}

func TestCreateBucketNeedsAProject(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)

	_, err := s.CreateBucket(context.Background(), storage.Bucket{Name: "orphan"})
	if got := status(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (err: %v)", got, err)
	}
}

// The namespace is global, as in GCS: a name taken in one project is taken in
// every other.
func TestBucketNamesAreGloballyUnique(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()

	makeBucket(t, s, ctx, "taken")
	_, err := s.CreateBucket(ctx, storage.Bucket{Name: "taken", Project: "other-project"})
	if got := status(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (err: %v)", got, err)
	}
}

// The name claim is rolled back when the metadata cannot be stored, or the
// name would stay taken by a bucket that does not exist.
func TestCreateBucketReleasesTheNameOnFailure(t *testing.T) {
	t.Parallel()
	s, fs, ctx := newFaulty(t)

	fs.failPut = onKeys(errBoom, bucketKey)
	_, err := s.CreateBucket(ctx, storage.Bucket{Name: "rollback", Project: "p"})
	wantInternal(t, err, "storing bucket metadata")

	// With the store healthy again the name is free.
	fs.failPut = nil
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "rollback", Project: "p"}); err != nil {
		t.Errorf("the name stayed claimed after a failed create: %v", err)
	}
}

func TestBucketStoreFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		arm      func(fs *faultyStore)
		call     func(s *storage.Service, ctx context.Context) error
		mentions string
	}{
		{
			"claiming the name",
			func(fs *faultyStore) { fs.failPut = onKeys(errBoom, indexKey) },
			func(s *storage.Service, ctx context.Context) error {
				_, err := s.CreateBucket(ctx, storage.Bucket{Name: "fresh", Project: "p"})
				return err
			},
			"claiming the bucket name",
		},
		{
			"resolving the project",
			func(fs *faultyStore) { fs.failGet = onKeys(errBoom, indexKey) },
			func(s *storage.Service, ctx context.Context) error {
				_, err := s.ProjectOf(ctx, "bkt")
				return err
			},
			"resolving the bucket's project",
		},
		{
			"reading the metadata",
			func(fs *faultyStore) { fs.failGet = onKeys(errBoom, bucketKey) },
			func(s *storage.Service, ctx context.Context) error {
				_, err := s.GetBucket(ctx, "p", "bkt")
				return err
			},
			"reading bucket metadata",
		},
		{
			"decoding the metadata",
			func(fs *faultyStore) { fs.tamper = tamperOn(bucketKey, []byte("{not json")) },
			func(s *storage.Service, ctx context.Context) error {
				_, err := s.GetBucket(ctx, "p", "bkt")
				return err
			},
			"decoding bucket metadata",
		},
		{
			"listing",
			func(fs *faultyStore) { fs.failList = onKeys(errBoom, bucketKey) },
			func(s *storage.Service, ctx context.Context) error {
				_, err := s.ListBuckets(ctx, "p")
				return err
			},
			"listing buckets",
		},
		{
			"checking whether the bucket is empty",
			func(fs *faultyStore) { fs.failList = onKeys(errBoom, bucketKey) },
			func(s *storage.Service, ctx context.Context) error {
				return s.DeleteBucket(ctx, "p", "bkt")
			},
			"checking whether the bucket is empty",
		},
		{
			"deleting the metadata",
			func(fs *faultyStore) { fs.failDelete = onKeys(errBoom, bucketKey) },
			func(s *storage.Service, ctx context.Context) error {
				return s.DeleteBucket(ctx, "p", "bkt")
			},
			"deleting bucket metadata",
		},
		{
			"releasing the name",
			func(fs *faultyStore) { fs.failDelete = onKeys(errBoom, indexKey) },
			func(s *storage.Service, ctx context.Context) error {
				return s.DeleteBucket(ctx, "p", "bkt")
			},
			"releasing the bucket name",
		},
		{
			"storing a patch",
			func(fs *faultyStore) { fs.failPut = onKeys(errBoom, bucketKey) },
			func(s *storage.Service, ctx context.Context) error {
				_, err := s.UpdateBucket(ctx, "p", "bkt", storage.BucketPatch{StorageClass: "NEARLINE"})
				return err
			},
			"storing bucket metadata",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, fs, ctx := newFaulty(t)
			makeBucket(t, s, ctx, "bkt")
			tc.arm(fs)

			wantInternal(t, tc.call(s, ctx), tc.mentions)
		})
	}
}

// A listing covers the bucket's contents too, so entries below it have to be
// skipped rather than decoded as buckets.
func TestListBucketsSkipsBucketContents(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	if _, err := write(t, s, "an/object", "hello", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	makeBucket(t, s, ctx, "second")

	got, err := s.ListBuckets(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("ListBuckets = %d buckets, want 2: %+v", len(got), got)
	}
}

func TestDeleteBucketRefusesANonEmptyOne(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	err := s.DeleteBucket(ctx, "p", "bkt")
	if got := status(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (err: %v)", got, err)
	}
}

func TestDeleteBucketOnAMissingOne(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)

	err := s.DeleteBucket(context.Background(), "p", "absent")
	if got := status(t, err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (err: %v)", got, err)
	}
}

// Metageneration moves on a patch, and the conditions are checked against what
// is currently there.
func TestUpdateBucketPreconditions(t *testing.T) {
	t.Parallel()

	t.Run("a patch bumps metageneration", func(t *testing.T) {
		t.Parallel()
		s, ctx := withBucket(t)
		on := true
		b, err := s.UpdateBucket(ctx, "p", "bkt", storage.BucketPatch{
			StorageClass: "NEARLINE", Versioning: &on,
		})
		if err != nil {
			t.Fatal(err)
		}
		if b.Metageneration != 2 || b.StorageClass != "NEARLINE" || !b.Versioning {
			t.Errorf("bucket = %+v", b)
		}
	})

	t.Run("a metageneration that does not match", func(t *testing.T) {
		t.Parallel()
		s, ctx := withBucket(t)
		_, err := s.UpdateBucket(ctx, "p", "bkt", storage.BucketPatch{
			Preconditions: storage.Preconditions{IfMetagenerationMatch: gen(99)},
		})
		if got := status(t, err); got != http.StatusPreconditionFailed {
			t.Errorf("status = %d, want 412 (err: %v)", got, err)
		}
	})

	t.Run("a NotMatch condition that holds", func(t *testing.T) {
		t.Parallel()
		s, ctx := withBucket(t)
		_, err := s.UpdateBucket(ctx, "p", "bkt", storage.BucketPatch{
			Preconditions: storage.Preconditions{IfMetagenerationNotMatch: gen(1)},
		})
		if got := status(t, err); got != http.StatusPreconditionFailed {
			t.Errorf("status = %d, want 412 (err: %v)", got, err)
		}
	})

	t.Run("a patch that lost the race", func(t *testing.T) {
		t.Parallel()
		s, fs, ctx := newFaulty(t)
		makeBucket(t, s, ctx, "bkt")
		fs.failPut = onKeys(store.ErrVersionMismatch, bucketKey)

		_, err := s.UpdateBucket(ctx, "p", "bkt", storage.BucketPatch{StorageClass: "NEARLINE"})
		if got := status(t, err); got != http.StatusPreconditionFailed {
			t.Errorf("status = %d, want 412 (err: %v)", got, err)
		}
	})
}

func TestUpdateBucketOnAMissingOne(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)

	_, err := s.UpdateBucket(context.Background(), "p", "absent", storage.BucketPatch{})
	if got := status(t, err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (err: %v)", got, err)
	}
}

func TestProjectOfAMissingBucket(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)

	if _, err := s.ProjectOf(context.Background(), "absent"); status(t, err) != http.StatusNotFound {
		t.Errorf("ProjectOf on a missing bucket = %v", err)
	}
}
