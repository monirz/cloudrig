package secretmanager

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestParseNames(t *testing.T) {
	t.Parallel()

	if _, _, err := parseSecret("projects/p/secrets/s"); err != nil {
		t.Errorf("a valid secret name was rejected: %v", err)
	}
	for _, bad := range []string{
		"", "projects/p", "projects/p/secrets", "projects//secrets/s",
		"projects/p/secrets/", "projects/p/buckets/s",
	} {
		if _, _, err := parseSecret(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("parseSecret(%q) = %v, want InvalidArgument", bad, err)
		}
	}

	secret, version, err := parseVersion("projects/p/secrets/s/versions/latest")
	if err != nil || secret != "projects/p/secrets/s" || version != "latest" {
		t.Errorf("parseVersion = %q, %q, %v", secret, version, err)
	}
	for _, bad := range []string{
		"projects/p/secrets/s", "projects/p/secrets/s/versions/",
		"projects/p/secrets/s/revisions/1",
	} {
		if _, _, err := parseVersion(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("parseVersion(%q) = %v, want InvalidArgument", bad, err)
		}
	}
}

// TestVersionKeysSortByNumber is what makes "the newest version" a listing
// rather than a scan: unpadded, version 10 would sort before version 2.
func TestVersionKeysSortByNumber(t *testing.T) {
	t.Parallel()

	if a, b := versionKey("s", 2), versionKey("s", 10); a >= b {
		t.Errorf("version 2 (%q) does not sort before version 10 (%q)", a, b)
	}
	if got := versionPrefix("projects/p/secrets/s"); got != "sm/v/projects/p/secrets/s/" {
		t.Errorf("versionPrefix = %q", got)
	}
}

// TestStoredVersionRoundTrips is the bug this package hit: a Replication is a
// oneof, and encoding/json cannot rebuild one. The envelope keeps the proto as
// protojson so it survives.
func TestStoredVersionRoundTrips(t *testing.T) {
	t.Parallel()

	original := storedVersion{
		Version: &secretmanagerpb.SecretVersion{
			Name:       "projects/p/secrets/s/versions/1",
			State:      secretmanagerpb.SecretVersion_ENABLED,
			CreateTime: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			ReplicationStatus: &secretmanagerpb.ReplicationStatus{
				ReplicationStatus: &secretmanagerpb.ReplicationStatus_Automatic{
					Automatic: &secretmanagerpb.ReplicationStatus_AutomaticStatus{},
				},
			},
		},
		Payload: []byte("value"),
	}

	encoded, err := original.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back storedVersion
	if err := back.UnmarshalJSON(encoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if string(back.Payload) != "value" {
		t.Errorf("payload = %q", back.Payload)
	}
	if back.Version.GetName() != original.Version.GetName() {
		t.Errorf("name = %q", back.Version.GetName())
	}
	if back.Version.GetReplicationStatus().GetAutomatic() == nil {
		t.Error("the replication oneof did not survive the round trip")
	}
}

// TestDeleteDoesNotLeaveAnOrphanVersion is a secret leak, not a tidiness
// problem: a version written while its secret was being deleted survived, and
// became readable the moment the name was recreated.
func TestDeleteDoesNotLeaveAnOrphanVersion(t *testing.T) {
	t.Parallel()

	automatic := &secretmanagerpb.Replication{
		Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{},
		},
	}
	const name = "projects/p/secrets/racy"
	create := &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: "racy",
		Secret: &secretmanagerpb.Secret{Replication: automatic},
	}

	for attempt := 0; attempt < 100; attempt++ {
		s := New(store.NewMemory(), clock.NewFake(epoch))
		ctx := context.Background()

		if _, err := s.CreateSecret(ctx, create); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("old")},
		}); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name})
		}()
		go func() {
			defer wg.Done()
			_, _ = s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
				Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("survivor")},
			})
		}()
		wg.Wait()

		// Recreating the name must not bring anything back with it.
		if _, err := s.CreateSecret(ctx, create); err != nil {
			t.Fatalf("attempt %d: recreating: %v", attempt, err)
		}
		got, err := s.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
			Name: name + "/versions/latest",
		})
		if err == nil {
			t.Fatalf("attempt %d: a recreated secret returned %q from before the delete",
				attempt, got.GetPayload().GetData())
		}
	}
}

// TestCreateDuringDeleteAdoptsNothing covers the other side of the lifecycle
// race: a secret recreated while an older one is being deleted must not
// inherit its values. Versions are removed before the name is freed, so there
// is no window in which the name exists beside them.
func TestCreateDuringDeleteAdoptsNothing(t *testing.T) {
	t.Parallel()

	automatic := &secretmanagerpb.Replication{
		Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{},
		},
	}
	const name = "projects/p/secrets/reused"
	create := &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: "reused",
		Secret: &secretmanagerpb.Secret{Replication: automatic},
	}

	for attempt := 0; attempt < 100; attempt++ {
		s := New(store.NewMemory(), clock.NewFake(epoch))
		ctx := context.Background()

		if _, err := s.CreateSecret(ctx, create); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("old-secret")},
		}); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var readErr error
		var read []byte
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = s.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name})
		}()
		go func() { defer wg.Done(); _, _ = s.CreateSecret(ctx, create) }()
		go func() {
			// Reading throughout, which is where the window would show.
			defer wg.Done()
			got, err := s.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
				Name: name + "/versions/latest",
			})
			readErr = err
			if err == nil {
				read = got.GetPayload().GetData()
			}
		}()
		wg.Wait()

		// Reading before the delete lands is fine; reading the old value
		// through a secret that was recreated is not.
		if readErr == nil && string(read) != "old-secret" {
			t.Fatalf("attempt %d: read %q", attempt, read)
		}
		if _, err := s.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
			Name: name + "/versions/latest",
		}); err == nil {
			t.Fatalf("attempt %d: values outlived the delete", attempt)
		}
	}
}

// TestDeleteReportsCleanupFailure holds that a delete which could not remove
// the values says so. Reporting success with payloads still stored is how a
// deleted credential comes back.
func TestDeleteReportsCleanupFailure(t *testing.T) {
	t.Parallel()

	s := New(failingDeletes{Store: store.NewMemory()}, clock.NewFake(epoch))
	ctx := context.Background()

	if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: "stuck",
		Secret: &secretmanagerpb.Secret{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  "projects/p/secrets/stuck",
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("v")},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := s.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{
		Name: "projects/p/secrets/stuck",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("err = %v, want the failure reported", err)
	}
	// And the secret is still there, so the caller can retry rather than
	// being left with a name that is gone and values that are not.
	if _, err := s.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{
		Name: "projects/p/secrets/stuck",
	}); err != nil {
		t.Errorf("the secret was removed despite the failure: %v", err)
	}
}

// failingDeletes refuses to delete a version, standing in for a store that
// fails partway.
type failingDeletes struct{ store.Store }

func (f failingDeletes) Delete(ctx context.Context, key string, ifVersion uint64) error {
	if strings.HasPrefix(key, "sm/v/") {
		return errors.New("the store refused")
	}
	return f.Store.Delete(ctx, key, ifVersion)
}
