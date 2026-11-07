package storage_test

import (
	"context"
	"testing"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/services/storage"
)

func TestManagedFolderName(t *testing.T) {
	for id, want := range map[string]string{"a": "a/", "a/b/": "a/b/", "/a/b": "a/b/"} {
		if got, err := storage.ManagedFolderName(id); err != nil || got != want {
			t.Errorf("ManagedFolderName(%q) = %q, %v; want %q", id, got, err, want)
		}
	}
	for _, id := range []string{"", "/", "a//b", "a\x00"} {
		if _, err := storage.ManagedFolderName(id); gerr.From(err).Code != gerr.InvalidArgument {
			t.Errorf("ManagedFolderName(%q) = %v, want InvalidArgument", id, err)
		}
	}
}

// A bucket holding a managed folder is not empty; once the folder is deleted
// the bucket goes, and a bucket made again under the name starts clean.
func TestDeleteManagedFolderThenBucket(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	mustBucket := func() {
		t.Helper()
		if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	mustBucket()
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "a/"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetManagedFolderPolicy(ctx, "p", "bkt", "a/", []byte("policy"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBucket(ctx, "p", "bkt"); gerr.From(err).Code != gerr.AlreadyExists {
		t.Fatalf("deleting a bucket with a folder = %v, want a 409 conflict", err)
	}

	if _, err := write(t, s, "a/x.txt", "x", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a", false); gerr.From(err).Code != gerr.FailedPrecondition {
		t.Errorf("deleting a non-empty folder = %v, want FailedPrecondition", err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a", true); err != nil {
		t.Fatalf("deleting with allowNonEmpty: %v", err)
	}
	if _, err := s.GetManagedFolder(ctx, "p", "bkt", "a/"); gerr.From(err).Code != gerr.NotFound {
		t.Errorf("get after delete = %v, want NotFound", err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a", true); gerr.From(err).Code != gerr.NotFound {
		t.Errorf("second delete = %v, want NotFound", err)
	}

	// The policy went with the folder.
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "a/"); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := s.ManagedFolderPolicy(ctx, "p", "bkt", "a/"); err != nil || raw != nil {
		t.Errorf("policy of a re-created folder = %q, %v; want none", raw, err)
	}
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "a"); gerr.From(err).Code != gerr.AlreadyExists {
		t.Errorf("repeat create = %v, want AlreadyExists", err)
	}
}
