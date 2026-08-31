package storage_test

import (
	"context"
	"testing"

	"github.com/monirz/cloudrig/services/storage"
)

func TestIAMPolicy(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	t.Run("an unset policy reads as empty", func(t *testing.T) {
		p, err := s.GetIAMPolicy(ctx, "p", "bkt", "")
		if err != nil {
			t.Fatal(err)
		}
		if p.Kind != "storage#policy" || p.ResourceID != "projects/_/buckets/bkt" {
			t.Errorf("policy = %+v", p)
		}
		if len(p.Bindings) != 0 {
			t.Errorf("bindings = %v, want none", p.Bindings)
		}
	})

	t.Run("set then get", func(t *testing.T) {
		want := storage.Policy{Bindings: []storage.Binding{
			{Role: "roles/storage.objectViewer", Members: []string{"allUsers"}},
		}}
		stored, err := s.SetIAMPolicy(ctx, "p", "bkt", "", want)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored.Bindings) != 1 || stored.Bindings[0].Role != "roles/storage.objectViewer" {
			t.Fatalf("stored = %+v", stored)
		}

		got, err := s.GetIAMPolicy(ctx, "p", "bkt", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "allUsers" {
			t.Errorf("read back = %+v", got)
		}
	})

	t.Run("the etag moves when the bindings do", func(t *testing.T) {
		first, _ := s.GetIAMPolicy(ctx, "p", "bkt", "")
		updated, err := s.SetIAMPolicy(ctx, "p", "bkt", "", storage.Policy{
			Bindings: []storage.Binding{
				{Role: "roles/storage.objectViewer", Members: []string{"allUsers", "allAuthenticatedUsers"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Etag == first.Etag {
			t.Error("the etag did not move with the bindings")
		}
	})

	t.Run("object policies are separate from the bucket's", func(t *testing.T) {
		if _, err := write(t, s, "obj", "content", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetIAMPolicy(ctx, "p", "bkt", "obj", storage.Policy{
			Bindings: []storage.Binding{{Role: "roles/storage.objectAdmin", Members: []string{"user:a@b.c"}}},
		}); err != nil {
			t.Fatal(err)
		}

		obj, _ := s.GetIAMPolicy(ctx, "p", "bkt", "obj")
		bucket, _ := s.GetIAMPolicy(ctx, "p", "bkt", "")
		if obj.Bindings[0].Role == bucket.Bindings[0].Role {
			t.Error("the object policy overwrote the bucket's")
		}
		if obj.ResourceID != "projects/_/buckets/bkt/objects/obj" {
			t.Errorf("resourceId = %q", obj.ResourceID)
		}
	})

	t.Run("a missing bucket is 404", func(t *testing.T) {
		if _, err := s.GetIAMPolicy(ctx, "p", "ghost", ""); status(t, err) != 404 {
			t.Errorf("status = %d, want 404", status(t, err))
		}
		_, err := s.SetIAMPolicy(context.Background(), "p", "ghost", "", storage.Policy{})
		if status(t, err) != 404 {
			t.Errorf("status = %d, want 404", status(t, err))
		}
	})
}
