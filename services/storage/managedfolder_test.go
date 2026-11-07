package storage_test

import (
	"context"
	"sync"
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
	if _, err := s.SetManagedFolderPolicy(ctx, "p", "bkt", "a/", []byte("policy"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBucket(ctx, "p", "bkt"); gerr.From(err).Code != gerr.AlreadyExists {
		t.Fatalf("deleting a bucket with a folder = %v, want a 409 conflict", err)
	}

	if _, err := write(t, s, "a/x.txt", "x", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a", storage.DeleteFolderOptions{}); gerr.From(err).Code != gerr.FailedPrecondition {
		t.Errorf("deleting a non-empty folder = %v, want FailedPrecondition", err)
	}
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "b/"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "b/c/"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "b/", storage.DeleteFolderOptions{}); gerr.From(err).Code != gerr.FailedPrecondition {
		t.Errorf("deleting a folder with one nested in it = %v, want FailedPrecondition", err)
	}
	for _, f := range []string{"b/c/", "b/"} { // innermost first, as gcloud does
		if err := s.DeleteManagedFolder(ctx, "p", "bkt", f, storage.DeleteFolderOptions{}); err != nil {
			t.Errorf("deleting %s: %v", f, err)
		}
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a", storage.DeleteFolderOptions{AllowNonEmpty: true}); err != nil {
		t.Fatalf("deleting with allowNonEmpty: %v", err)
	}
	if _, err := s.GetManagedFolder(ctx, "p", "bkt", "a/"); gerr.From(err).Code != gerr.NotFound {
		t.Errorf("get after delete = %v, want NotFound", err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a", storage.DeleteFolderOptions{AllowNonEmpty: true}); gerr.From(err).Code != gerr.NotFound {
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

// An etag read before a folder is deleted and created again must not match
// the new folder's policy, even though its store version starts over.
func TestPolicyEtagSurvivesRecreate(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	mk := func() {
		t.Helper()
		if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "x/"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetManagedFolderPolicy(ctx, "p", "bkt", "x/", []byte("v"), nil); err != nil {
			t.Fatal(err)
		}
	}
	mk()
	_, stale, err := s.ManagedFolderPolicy(ctx, "p", "bkt", "x/")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "x/", storage.DeleteFolderOptions{}); err != nil {
		t.Fatal(err)
	}
	mk()

	if _, err := s.SetManagedFolderPolicy(ctx, "p", "bkt", "x/", []byte("w"), stale); gerr.From(err).Code != gerr.Aborted {
		t.Errorf("set with an etag from the deleted folder = %v, want Aborted", err)
	}
	_, fresh, _ := s.ManagedFolderPolicy(ctx, "p", "bkt", "x/")
	if _, err := s.SetManagedFolderPolicy(ctx, "p", "bkt", "x/", []byte("w"), fresh); err != nil {
		t.Errorf("set with the current etag: %v", err)
	}
}

// A delete's metageneration conditions are checked before anything goes.
func TestDeleteManagedFolderConditions(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "a/"); err != nil {
		t.Fatal(err)
	}
	one, two := int64(1), int64(2)
	for _, opts := range []storage.DeleteFolderOptions{
		{IfMetagenerationMatch: &two},
		{IfMetagenerationNotMatch: &one},
	} {
		if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a/", opts); gerr.From(err).Code != gerr.FailedPrecondition {
			t.Errorf("delete with an unmet condition = %v, want FailedPrecondition", err)
		}
	}
	if _, err := s.GetManagedFolder(ctx, "p", "bkt", "a/"); err != nil {
		t.Fatalf("folder gone after a refused delete: %v", err)
	}
	if err := s.DeleteManagedFolder(ctx, "p", "bkt", "a/", storage.DeleteFolderOptions{IfMetagenerationMatch: &one, IfMetagenerationNotMatch: &two}); err != nil {
		t.Errorf("delete with met conditions: %v", err)
	}
}

// A policy write racing a delete must not leave the policy behind for a
// later create of the same name to inherit.
func TestPolicyWriteRacingDelete(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "r/"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = s.SetManagedFolderPolicy(ctx, "p", "bkt", "r/", []byte("old"), nil) })
		if err := s.DeleteManagedFolder(ctx, "p", "bkt", "r/", storage.DeleteFolderOptions{}); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "r/"); err != nil {
			t.Fatal(err)
		}
		if raw, _, err := s.ManagedFolderPolicy(ctx, "p", "bkt", "r/"); err != nil || raw != nil {
			t.Fatalf("a re-created folder has policy %q, %v; want none", raw, err)
		}
		if err := s.DeleteManagedFolder(ctx, "p", "bkt", "r/", storage.DeleteFolderOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}
