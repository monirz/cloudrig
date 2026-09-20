package secretmanager

import (
	"context"
	"errors"
	"hash/crc32"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

func newSM(t *testing.T) (*Service, context.Context) {
	t.Helper()
	return New(store.NewMemory(), clock.NewFake(epoch)), context.Background()
}

// seed creates a secret with one version and returns the secret's name.
func seed(t *testing.T, s *Service, ctx context.Context, id, value string) string {
	t.Helper()
	if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: id,
	}); err != nil {
		t.Fatal(err)
	}
	name := "projects/p/secrets/" + id
	if _, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)},
	}); err != nil {
		t.Fatal(err)
	}
	return name
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("code = %v, want %v (err: %v)", got, want, err)
	}
}

func TestCreateSecretRejectsBadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, parent, id string
	}{
		{"no parent", "", "s"},
		{"a parent with extra path", "projects/p/locations/l", "s"},
		{"no secret id", "projects/p", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := newSM(t)
			_, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
				Parent: tc.parent, SecretId: tc.id,
			})
			wantCode(t, err, codes.InvalidArgument)
		})
	}
}

// The name is claimed in one step, so the second create loses rather than
// overwriting the first.
func TestCreateSecretRefusesADuplicate(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)

	req := &secretmanagerpb.CreateSecretRequest{Parent: "projects/p", SecretId: "dup"}
	if _, err := s.CreateSecret(ctx, req); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateSecret(ctx, req)
	wantCode(t, err, codes.AlreadyExists)
}

// A create with no replication is given automatic replication, because that is
// the only thing a local emulator can honestly claim.
func TestCreateSecretDefaultsReplication(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)

	secret, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if secret.GetReplication().GetAutomatic() == nil {
		t.Errorf("replication = %v, want automatic", secret.GetReplication())
	}
}

func TestSecretNamesAreValidated(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)

	const bad = "projects/p/buckets/s"
	if _, err := s.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetSecret(%q) = %v", bad, err)
	}
	if _, err := s.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteSecret(%q) = %v", bad, err)
	}
	if _, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("AddSecretVersion(%q) = %v", bad, err)
	}
}

func TestListSecrets(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)

	seed(t, s, ctx, "b", "1")
	seed(t, s, ctx, "a", "2")

	out, err := s.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: "projects/p"})
	if err != nil {
		t.Fatal(err)
	}
	if out.GetTotalSize() != 2 {
		t.Fatalf("TotalSize = %d, want 2", out.GetTotalSize())
	}
	// Sorted by name, so a listing is stable between calls.
	if !strings.HasSuffix(out.GetSecrets()[0].GetName(), "/a") {
		t.Errorf("secrets are not name-ordered: %v", out.GetSecrets())
	}
}

// A checksum the client sent has to match, and is confirmed back when it does:
// gcloud reports corruption if the confirmation is missing.
func TestAddSecretVersionChecksum(t *testing.T) {
	t.Parallel()

	castagnoli := crc32.MakeTable(crc32.Castagnoli)

	t.Run("a matching checksum is confirmed", func(t *testing.T) {
		t.Parallel()
		s, ctx := newSM(t)
		if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
			Parent: "projects/p", SecretId: "s",
		}); err != nil {
			t.Fatal(err)
		}
		sum := int64(crc32.Checksum([]byte("value"), castagnoli))
		v, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent:  "projects/p/secrets/s",
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("value"), DataCrc32C: &sum},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !v.GetClientSpecifiedPayloadChecksum() {
			t.Error("the honoured checksum was not confirmed back")
		}
	})

	t.Run("a wrong checksum is refused", func(t *testing.T) {
		t.Parallel()
		s, ctx := newSM(t)
		if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
			Parent: "projects/p", SecretId: "s",
		}); err != nil {
			t.Fatal(err)
		}
		wrong := int64(1)
		_, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent:  "projects/p/secrets/s",
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("value"), DataCrc32C: &wrong},
		})
		wantCode(t, err, codes.InvalidArgument)
	})
}

func TestAddSecretVersionNeedsTheSecret(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)

	_, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  "projects/p/secrets/absent",
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("v")},
	})
	wantCode(t, err, codes.NotFound)
}

// Revoking a value is not deleting it: the version still exists, and still
// refuses to be read.
func TestAccessRefusesADisabledVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		kill func(*Service, context.Context, string) error
	}{
		{"disabled", func(s *Service, ctx context.Context, v string) error {
			_, err := s.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v})
			return err
		}},
		{"destroyed", func(s *Service, ctx context.Context, v string) error {
			_, err := s.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := newSM(t)
			name := seed(t, s, ctx, "s", "value")
			version := name + "/versions/1"

			if err := tc.kill(s, ctx, version); err != nil {
				t.Fatal(err)
			}
			_, err := s.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version})
			wantCode(t, err, codes.FailedPrecondition)

			// It still exists: that is the difference from a delete.
			if _, err := s.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{
				Name: version,
			}); err != nil {
				t.Errorf("the version was removed, not revoked: %v", err)
			}
		})
	}
}

func TestVersionNamesAreValidated(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)

	const bad = "projects/p/secrets/s/nope/1"
	if _, err := s.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: bad,
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("AccessSecretVersion(%q) = %v", bad, err)
	}
	if _, err := s.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{
		Name: bad,
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetSecretVersion(%q) = %v", bad, err)
	}
}

func TestListSecretVersions(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)
	name := seed(t, s, ctx, "s", "one")

	if _, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("two")},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := s.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: name})
	if err != nil {
		t.Fatal(err)
	}
	if out.GetTotalSize() != 2 {
		t.Fatalf("TotalSize = %d, want 2", out.GetTotalSize())
	}
	// Newest first, as the API returns them.
	if !strings.HasSuffix(out.GetVersions()[0].GetName(), "/2") {
		t.Errorf("versions are not newest-first: %v", out.GetVersions())
	}
}

func TestListSecretVersionsNeedsTheSecret(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)

	_, err := s.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{
		Parent: "projects/p/secrets/absent",
	})
	wantCode(t, err, codes.NotFound)
}

// Numbers are never reused, so a reference to a version always means the same
// bytes even after the version it followed is destroyed.
func TestVersionNumbersAreNeverReused(t *testing.T) {
	t.Parallel()
	s, ctx := newSM(t)
	name := seed(t, s, ctx, "s", "one")

	if _, err := s.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{
		Name: name + "/versions/1",
	}); err != nil {
		t.Fatal(err)
	}
	v, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("two")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(v.GetName(), "/versions/2") {
		t.Errorf("name = %q, want version 2", v.GetName())
	}
}

func TestVersionNumber(t *testing.T) {
	t.Parallel()

	if n, err := versionNumber("projects/p/secrets/s/versions/7"); err != nil || n != 7 {
		t.Errorf("versionNumber = %d, %v; want 7", n, err)
	}
	if _, err := versionNumber("projects/p/secrets/s/versions/latest"); status.Code(err) != codes.Internal {
		t.Errorf("a non-numeric version was accepted: %v", err)
	}
}

// failingLists refuses to list under a prefix, standing in for a broken store.
type failingLists struct {
	store.Store
	prefix string
}

func (f failingLists) List(ctx context.Context, prefix string, limit int, token string) ([]store.KV, string, error) {
	if strings.HasPrefix(prefix, f.prefix) {
		return nil, "", errors.New("the store refused")
	}
	return f.Store.List(ctx, prefix, limit, token)
}

func TestStoreFailuresAreReported(t *testing.T) {
	t.Parallel()

	t.Run("listing secrets", func(t *testing.T) {
		t.Parallel()
		s := New(failingLists{Store: store.NewMemory(), prefix: "sm/s/"}, clock.NewFake(epoch))
		_, err := s.ListSecrets(context.Background(), &secretmanagerpb.ListSecretsRequest{
			Parent: "projects/p",
		})
		wantCode(t, err, codes.Internal)
	})

	t.Run("listing versions while numbering a new one", func(t *testing.T) {
		t.Parallel()
		kv := store.NewMemory()
		s := New(kv, clock.NewFake(epoch))
		ctx := context.Background()
		if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
			Parent: "projects/p", SecretId: "s",
		}); err != nil {
			t.Fatal(err)
		}

		broken := New(failingLists{Store: kv, prefix: "sm/v/"}, clock.NewFake(epoch))
		_, err := broken.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent:  "projects/p/secrets/s",
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("v")},
		})
		wantCode(t, err, codes.Internal)
	})
}

// failingPuts refuses to write under a prefix, standing in for a store that is
// readable but not writable.
type failingPuts struct {
	store.Store
	prefix string
}

func (f failingPuts) Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) {
	if strings.HasPrefix(key, f.prefix) {
		return 0, errors.New("the store refused")
	}
	return f.Store.Put(ctx, key, val, ifVersion)
}

// emptyLists hides what is under a prefix without erroring, which is how a
// version number gets recomputed over a key that is already taken.
type emptyLists struct {
	store.Store
	prefix string
}

func (e emptyLists) List(ctx context.Context, prefix string, limit int, token string) ([]store.KV, string, error) {
	if strings.HasPrefix(prefix, e.prefix) {
		return nil, "", nil
	}
	return e.Store.List(ctx, prefix, limit, token)
}

func TestWriteFailuresAreReported(t *testing.T) {
	t.Parallel()

	t.Run("storing a secret", func(t *testing.T) {
		t.Parallel()
		s := New(failingPuts{Store: store.NewMemory(), prefix: "sm/s/"}, clock.NewFake(epoch))
		_, err := s.CreateSecret(context.Background(), &secretmanagerpb.CreateSecretRequest{
			Parent: "projects/p", SecretId: "s",
		})
		wantCode(t, err, codes.Internal)
	})

	t.Run("storing a version", func(t *testing.T) {
		t.Parallel()
		kv := store.NewMemory()
		s, ctx := New(kv, clock.NewFake(epoch)), context.Background()
		if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
			Parent: "projects/p", SecretId: "s",
		}); err != nil {
			t.Fatal(err)
		}

		broken := New(failingPuts{Store: kv, prefix: "sm/v/"}, clock.NewFake(epoch))
		_, err := broken.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent:  "projects/p/secrets/s",
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("v")},
		})
		wantCode(t, err, codes.Internal)
	})

	t.Run("a version number that is already taken", func(t *testing.T) {
		t.Parallel()
		kv := store.NewMemory()
		s, ctx := New(kv, clock.NewFake(epoch)), context.Background()
		seed(t, s, ctx, "s", "one")

		// The listing is hidden, so the next number lands on version 1 again.
		blind := New(emptyLists{Store: kv, prefix: "sm/v/"}, clock.NewFake(epoch))
		_, err := blind.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent:  "projects/p/secrets/s",
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("two")},
		})
		wantCode(t, err, codes.Aborted)
	})

	t.Run("changing a version's state", func(t *testing.T) {
		t.Parallel()
		kv := store.NewMemory()
		s, ctx := New(kv, clock.NewFake(epoch)), context.Background()
		name := seed(t, s, ctx, "s", "one")

		broken := New(failingPuts{Store: kv, prefix: "sm/v/"}, clock.NewFake(epoch))
		_, err := broken.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{
			Name: name + "/versions/1",
		})
		wantCode(t, err, codes.Aborted)
	})
}

// refusesDeletes refuses to remove anything under a prefix. failingDeletes
// covers the version tree; this one is aimed at the name itself.
type refusesDeletes struct {
	store.Store
	prefix string
}

func (r refusesDeletes) Delete(ctx context.Context, key string, ifVersion uint64) error {
	if strings.HasPrefix(key, r.prefix) {
		return errors.New("the store refused")
	}
	return r.Store.Delete(ctx, key, ifVersion)
}

// A secret whose name cannot be removed is reported as missing rather than
// silently left behind.
func TestDeleteReportsAFailedNameRemoval(t *testing.T) {
	t.Parallel()

	kv := store.NewMemory()
	s, ctx := New(kv, clock.NewFake(epoch)), context.Background()
	if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: "s",
	}); err != nil {
		t.Fatal(err)
	}

	broken := New(refusesDeletes{Store: kv, prefix: "sm/s/"}, clock.NewFake(epoch))
	_, err := broken.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{
		Name: "projects/p/secrets/s",
	})
	wantCode(t, err, codes.NotFound)
}
