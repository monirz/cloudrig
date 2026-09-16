package secretmanager

import (
	"context"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// TestGetSecretVersion covers reading a version's metadata (as opposed to its
// payload, which AccessSecretVersion returns).
func TestGetSecretVersion(t *testing.T) {
	t.Parallel()

	s := New(store.NewMemory(), clock.NewFake(epoch))
	ctx := context.Background()
	automatic := &secretmanagerpb.Replication{
		Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{},
		},
	}
	const name = "projects/p/secrets/api-key"

	if _, err := s.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/p", SecretId: "api-key",
		Secret: &secretmanagerpb.Secret{Replication: automatic},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: name, Payload: &secretmanagerpb.SecretPayload{Data: []byte("s3cret")},
	}); err != nil {
		t.Fatal(err)
	}

	v, err := s.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{
		Name: name + "/versions/latest",
	})
	if err != nil {
		t.Fatalf("GetSecretVersion: %v", err)
	}
	if v.GetName() == "" {
		t.Error("GetSecretVersion returned a version with no name")
	}
}
