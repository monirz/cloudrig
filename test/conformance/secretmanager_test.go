package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// post sends a JSON request to the emulator and returns the decoded reply.
func post(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: body %q: %v", method, url, raw, err)
		}
	}
	return resp.StatusCode, out
}

// TestSecretRESTLifecycle is the surface gcloud uses: create, add a version,
// read it back, and the :verb forms that ride on the name segment.
func TestSecretRESTLifecycle(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/secrets"

	if code, _ := post(t, http.MethodPost, base+"?secretId=cfg",
		`{"replication":{"automatic":{}}}`); code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}

	code, version := post(t, http.MethodPost, base+"/cfg:addVersion",
		`{"payload":{"data":"czNjcjN0"}}`)
	if code != http.StatusOK {
		t.Fatalf("addVersion = %d %v", code, version)
	}
	if version["name"] != "projects/p/secrets/cfg/versions/1" {
		t.Errorf("version name = %v", version["name"])
	}

	code, accessed := post(t, http.MethodGet, base+"/cfg/versions/latest:access", "")
	if code != http.StatusOK {
		t.Fatalf("access = %d %v", code, accessed)
	}
	payload, _ := accessed["payload"].(map[string]any)
	if payload["data"] != "czNjcjN0" {
		t.Errorf("payload = %v", payload)
	}

	if code, _ := post(t, http.MethodPost, base+"/cfg/versions/1:disable", ""); code != http.StatusOK {
		t.Errorf("disable = %d", code)
	}
	// A disabled version refuses to be read, and there is no other version to
	// fall back to.
	if code, _ := post(t, http.MethodGet, base+"/cfg/versions/1:access", ""); code == http.StatusOK {
		t.Error("a disabled version was readable")
	}

	if code, _ := post(t, http.MethodDelete, base+"/cfg", ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
	if code, _ := post(t, http.MethodGet, base+"/cfg", ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d", code)
	}
}

// TestSecretChecksumIsConfirmed holds a field gcloud reads to decide whether
// the value it sent arrived intact. Without it every `gcloud secrets versions
// add` reports data corruption, however intact the value is.
func TestSecretChecksumIsConfirmed(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/secrets"
	post(t, http.MethodPost, base+"?secretId=summed", `{"replication":{"automatic":{}}}`)

	// crc32c("s3cr3t"), Castagnoli.
	code, version := post(t, http.MethodPost, base+"/summed:addVersion",
		`{"payload":{"data":"czNjcjN0","dataCrc32c":"825573743"}}`)
	if code != http.StatusOK {
		t.Fatalf("addVersion = %d %v", code, version)
	}
	if version["clientSpecifiedPayloadChecksum"] != true {
		t.Errorf("the checksum was not confirmed back: %v", version)
	}

	// A checksum that does not match the data is refused.
	if code, _ := post(t, http.MethodPost, base+"/summed:addVersion",
		`{"payload":{"data":"czNjcjN0","dataCrc32c":"1"}}`); code != http.StatusBadRequest {
		t.Errorf("a wrong checksum = %d, want 400", code)
	}
}

// TestSecretRESTAndGRPCShareState is the claim worth holding: one service
// behind two APIs.
func TestSecretRESTAndGRPCShareState(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := secretmanager.NewClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Made over REST, the way gcloud would.
	base := emu.BaseURL() + "/v1/projects/test-project/secrets"
	post(t, http.MethodPost, base+"?secretId=shared", `{"replication":{"automatic":{}}}`)
	post(t, http.MethodPost, base+"/shared:addVersion", `{"payload":{"data":"czNjcjN0"}}`)

	got, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: "projects/test-project/secrets/shared/versions/latest",
	})
	if err != nil {
		t.Fatalf("a secret made over REST is invisible to gRPC: %v", err)
	}
	if string(got.GetPayload().GetData()) != "s3cr3t" {
		t.Errorf("value = %q", got.GetPayload().GetData())
	}
}

// TestSecretUpdateTouchesNamedFields is what `gcloud secrets update` needs. A
// masked update must leave alone what it did not mention.
func TestSecretUpdateTouchesNamedFields(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/secrets"
	post(t, http.MethodPost, base+"?secretId=labelled",
		`{"replication":{"automatic":{}},"labels":{"env":"local","team":"core"}}`)

	code, updated := post(t, http.MethodPatch, base+"/labelled?updateMask=labels",
		`{"labels":{"env":"staging","team":"core"}}`)
	if code != http.StatusOK {
		t.Fatalf("update = %d %v", code, updated)
	}

	labels, _ := updated["labels"].(map[string]any)
	if labels["env"] != "staging" {
		t.Errorf("labels = %v", labels)
	}
	// The replication was never mentioned, so it survives.
	if updated["replication"] == nil {
		t.Errorf("a field the mask did not name was cleared: %v", updated)
	}

	// A mask naming a field that does not exist is a client error, and the
	// name cannot be changed at all.
	if code, _ := post(t, http.MethodPatch, base+"/labelled?updateMask=nonsense", `{}`); code != http.StatusBadRequest {
		t.Errorf("an unknown mask field = %d, want 400", code)
	}
	if code, _ := post(t, http.MethodPatch, base+"/labelled?updateMask=name", `{}`); code != http.StatusBadRequest {
		t.Errorf("renaming = %d, want 400", code)
	}
}

// TestSecretIamVerbs holds that the IAM forms answer rather than reporting the
// secret missing. They arrive glued to the name segment, and reading them as
// part of the name made a live secret look absent.
func TestSecretIamVerbs(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/secrets"
	post(t, http.MethodPost, base+"?secretId=guarded", `{"replication":{"automatic":{}}}`)

	code, policy := post(t, http.MethodGet, base+"/guarded:getIamPolicy", "")
	if code != http.StatusOK {
		t.Fatalf("getIamPolicy = %d %v", code, policy)
	}
	if policy["etag"] == nil {
		t.Errorf("no policy returned: %v", policy)
	}

	if code, _ := post(t, http.MethodPost, base+"/guarded:setIamPolicy", `{"policy":{}}`); code != http.StatusOK {
		t.Errorf("setIamPolicy = %d", code)
	}

	code, tested := post(t, http.MethodPost, base+"/guarded:testIamPermissions",
		`{"permissions":["secretmanager.versions.access"]}`)
	if code != http.StatusOK {
		t.Fatalf("testIamPermissions = %d", code)
	}
	granted, _ := tested["permissions"].([]any)
	if len(granted) != 1 {
		t.Errorf("permissions = %v, want what was asked for", tested)
	}
}

// TestSecretRejectsAnOversizedBody keeps one unauthenticated request from
// allocating until the emulator dies. The port is shared, so that would take
// every other service down with it.
func TestSecretRejectsAnOversizedBody(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/secrets"

	// Comfortably past the cap, and still small enough to send quickly.
	huge := `{"labels":{"x":"` + strings.Repeat("A", 5<<20) + `"}}`
	code, _ := post(t, http.MethodPost, base+"?secretId=big", huge)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", code)
	}

	// Every route that reads a body is capped, not only the ones decoding a
	// proto: the IAM verbs took their own path and slipped the limit.
	if code, _ := post(t, http.MethodPost, base+"/small:testIamPermissions",
		`{"permissions":["`+strings.Repeat("B", 5<<20)+`"]}`); code != http.StatusRequestEntityTooLarge {
		t.Errorf("testIamPermissions with an oversized body = %d, want 413", code)
	}

	// An ordinary body still works.
	if code, _ := post(t, http.MethodPost, base+"?secretId=small",
		`{"replication":{"automatic":{}}}`); code != http.StatusOK {
		t.Errorf("a normal create = %d", code)
	}
}
