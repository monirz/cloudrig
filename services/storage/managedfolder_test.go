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

// A bucket deleted and made again under the same name starts with no folders
// and no folder policies.
func TestDeleteBucketDropsManagedFolders(t *testing.T) {
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
	if err := s.DeleteBucket(ctx, "p", "bkt"); err != nil {
		t.Fatal(err)
	}
	mustBucket()

	folders, _, err := s.ListManagedFolders(ctx, "p", "bkt", "", 0, "")
	if err != nil || len(folders) != 0 {
		t.Fatalf("folders after re-create = %v, %v; want none", folders, err)
	}
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "a/"); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := s.ManagedFolderPolicy(ctx, "p", "bkt", "a/"); err != nil || raw != nil {
		t.Errorf("policy after re-create = %q, %v; want none", raw, err)
	}
	if _, err := s.CreateManagedFolder(ctx, "p", "bkt", "a"); gerr.From(err).Code != gerr.AlreadyExists {
		t.Errorf("repeat create = %v, want AlreadyExists", err)
	}
}
