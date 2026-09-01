package conformance

import (
	"context"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/monirz/cloudrig"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func smClient(t *testing.T) (*secretmanager.Client, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := secretmanager.NewClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("secretmanager.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

// mkSecret creates a secret and returns its resource name.
func mkSecret(t *testing.T, c *secretmanager.Client, ctx context.Context, id string) string {
	t.Helper()

	secret, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   "projects/test-project",
		SecretId: id,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	return secret.GetName()
}

func addVersion(t *testing.T, c *secretmanager.Client, ctx context.Context, secret, value string) string {
	t.Helper()

	version, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  secret,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte(value)},
	})
	if err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	return version.GetName()
}

func access(t *testing.T, c *secretmanager.Client, ctx context.Context, name string) string {
	t.Helper()

	got, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		t.Fatalf("AccessSecretVersion(%s): %v", name, err)
	}
	return string(got.GetPayload().GetData())
}

// TestSecretRoundTrip is the call everything else exists to serve: store a
// value, read it back.
func TestSecretRoundTrip(t *testing.T) {
	c, ctx := smClient(t)

	secret := mkSecret(t, c, ctx, "api-key")
	if secret != "projects/test-project/secrets/api-key" {
		t.Errorf("name = %q", secret)
	}

	version := addVersion(t, c, ctx, secret, "s3cr3t")
	if version != secret+"/versions/1" {
		t.Errorf("version = %q, want it numbered from one", version)
	}
	if got := access(t, c, ctx, version); got != "s3cr3t" {
		t.Errorf("value = %q", got)
	}
}

// TestLatestFollowsTheNewestVersion is the indirection the whole model exists
// for: rotating a secret must not need the code reading it to change.
func TestLatestFollowsTheNewestVersion(t *testing.T) {
	c, ctx := smClient(t)

	secret := mkSecret(t, c, ctx, "rotating")
	addVersion(t, c, ctx, secret, "first")
	if got := access(t, c, ctx, secret+"/versions/latest"); got != "first" {
		t.Errorf("latest = %q, want first", got)
	}

	addVersion(t, c, ctx, secret, "second")
	if got := access(t, c, ctx, secret+"/versions/latest"); got != "second" {
		t.Errorf("latest = %q after rotating, want second", got)
	}
	// The old version is still readable by number.
	if got := access(t, c, ctx, secret+"/versions/1"); got != "first" {
		t.Errorf("version 1 = %q", got)
	}
}

// TestDisableMovesLatestBack covers the rotation that went wrong: disabling
// the newest version has to move the alias, or a revoked value keeps serving.
func TestDisableMovesLatestBack(t *testing.T) {
	c, ctx := smClient(t)

	secret := mkSecret(t, c, ctx, "revoked")
	addVersion(t, c, ctx, secret, "good")
	bad := addVersion(t, c, ctx, secret, "bad")

	if _, err := c.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{
		Name: bad,
	}); err != nil {
		t.Fatalf("DisableSecretVersion: %v", err)
	}

	if got := access(t, c, ctx, secret+"/versions/latest"); got != "good" {
		t.Errorf("latest = %q after disabling the newest, want good", got)
	}
	// The disabled version still exists, and still refuses to be read.
	_, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: bad})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("accessing a disabled version = %v, want FailedPrecondition", err)
	}

	// Enabling it puts it back in front.
	if _, err := c.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{
		Name: bad,
	}); err != nil {
		t.Fatal(err)
	}
	if got := access(t, c, ctx, secret+"/versions/latest"); got != "bad" {
		t.Errorf("latest = %q after re-enabling", got)
	}
}

// TestDestroyDiscardsTheValue is what separates destroy from disable.
func TestDestroyDiscardsTheValue(t *testing.T) {
	c, ctx := smClient(t)

	secret := mkSecret(t, c, ctx, "gone")
	version := addVersion(t, c, ctx, secret, "burn-after-reading")

	destroyed, err := c.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{
		Name: version,
	})
	if err != nil {
		t.Fatalf("DestroySecretVersion: %v", err)
	}
	if destroyed.GetState() != secretmanagerpb.SecretVersion_DESTROYED {
		t.Errorf("state = %v", destroyed.GetState())
	}
	if destroyed.GetDestroyTime() == nil {
		t.Error("no destroy time")
	}

	// A destroyed version keeps its number and loses its value, and cannot be
	// brought back.
	if _, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: version,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("accessing a destroyed version = %v", err)
	}
	if _, err := c.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{
		Name: version,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("re-enabling a destroyed version = %v, want it refused", err)
	}
}

// TestVersionNumbersAreNeverReused holds that a reference to a version always
// means the same bytes.
func TestVersionNumbersAreNeverReused(t *testing.T) {
	c, ctx := smClient(t)

	secret := mkSecret(t, c, ctx, "numbered")
	first := addVersion(t, c, ctx, secret, "one")
	if _, err := c.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{
		Name: first,
	}); err != nil {
		t.Fatal(err)
	}

	if second := addVersion(t, c, ctx, secret, "two"); second != secret+"/versions/2" {
		t.Errorf("the next version is %q; a destroyed number must not be reused", second)
	}
}

func TestSecretErrors(t *testing.T) {
	c, ctx := smClient(t)

	mkSecret(t, c, ctx, "dup")
	if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/test-project", SecretId: "dup",
		Secret: &secretmanagerpb.Secret{},
	}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate create = %v, want AlreadyExists", err)
	}

	if _, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{
		Name: "projects/test-project/secrets/ghost",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("missing secret = %v, want NotFound", err)
	}

	// A version on a secret that does not exist.
	if _, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  "projects/test-project/secrets/ghost",
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")},
	}); status.Code(err) != codes.NotFound {
		t.Errorf("adding to a missing secret = %v, want NotFound", err)
	}

	// A secret with no versions has no latest.
	empty := mkSecret(t, c, ctx, "empty")
	if _, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: empty + "/versions/latest",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("latest of an empty secret = %v, want NotFound", err)
	}
}

// TestListSecretsAndVersions covers what a console or a script enumerates.
func TestListSecretsAndVersions(t *testing.T) {
	c, ctx := smClient(t)

	secret := mkSecret(t, c, ctx, "listed")
	mkSecret(t, c, ctx, "listed-too")
	addVersion(t, c, ctx, secret, "a")
	addVersion(t, c, ctx, secret, "b")

	secrets := c.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{
		Parent: "projects/test-project",
	})
	var names []string
	for {
		s, err := secrets.Next()
		if err != nil {
			break
		}
		names = append(names, s.GetName())
	}
	if len(names) != 2 {
		t.Errorf("secrets = %v, want 2", names)
	}

	versions := c.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: secret})
	var count int
	for {
		if _, err := versions.Next(); err != nil {
			break
		}
		count++
	}
	if count != 2 {
		t.Errorf("versions = %d, want 2", count)
	}
}
