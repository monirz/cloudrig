package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// do sends a request to the emulator's JSON API and decodes the reply. This is
// the surface Terraform uses; the Go client never touches it.
func do(t *testing.T, method, url, body string) (int, map[string]any) {
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

// TestPubSubRESTLifecycle walks what Terraform does: create, read, update
// through a field mask, then delete.
func TestPubSubRESTLifecycle(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/topics/orders"

	if code, _ := do(t, http.MethodPut, base, `{}`); code != http.StatusOK {
		t.Fatalf("create = %d, want 200", code)
	}

	code, got := do(t, http.MethodGet, base, "")
	if code != http.StatusOK || got["name"] != "projects/p/topics/orders" {
		t.Fatalf("get = %d %v", code, got)
	}

	// The mask names the field in the spelling REST uses.
	code, got = do(t, http.MethodPatch, base,
		`{"topic":{"labels":{"env":"local"}},"updateMask":"labels"}`)
	if code != http.StatusOK {
		t.Fatalf("patch = %d %v", code, got)
	}
	labels, _ := got["labels"].(map[string]any)
	if labels["env"] != "local" {
		t.Errorf("labels = %v, want env=local", got["labels"])
	}

	if code, _ := do(t, http.MethodDelete, base, ""); code != http.StatusOK {
		t.Errorf("delete = %d, want 200", code)
	}
	if code, _ := do(t, http.MethodGet, base, ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// TestPubSubRESTErrors pins the codes Terraform reads to decide whether a
// resource exists.
func TestPubSubRESTErrors(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	topic := emu.BaseURL() + "/v1/projects/p/topics/dup"

	do(t, http.MethodPut, topic, `{}`)
	if code, _ := do(t, http.MethodPut, topic, `{}`); code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", code)
	}
	if code, _ := do(t, http.MethodGet, emu.BaseURL()+"/v1/projects/p/topics/ghost", ""); code != http.StatusNotFound {
		t.Errorf("missing topic = %d, want 404", code)
	}
	// An update mask naming a field that does not exist is a client error.
	code, _ := do(t, http.MethodPatch, topic, `{"topic":{},"updateMask":"nonsense"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad mask = %d, want 400", code)
	}
}

// TestPubSubRESTAndGRPCShareState is the claim worth holding: one service
// behind two APIs, not two stores that agree by luck.
func TestPubSubRESTAndGRPCShareState(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	// Made over REST, the way Terraform would.
	if code, _ := do(t, http.MethodPut,
		emu.BaseURL()+"/v1/projects/test-project/topics/shared", `{}`); code != http.StatusOK {
		t.Fatalf("REST create = %d", code)
	}

	c, err := pubsub.NewClient(ctx, "test-project",
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	const topic = "projects/test-project/topics/shared"
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); err != nil {
		t.Fatalf("a topic made over REST is invisible to gRPC: %v", err)
	}

	// And the reverse: a subscription made over gRPC is readable over REST.
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: "projects/test-project/subscriptions/both", Topic: topic,
	}); err != nil {
		t.Fatal(err)
	}
	code, got := do(t, http.MethodGet,
		emu.BaseURL()+"/v1/projects/test-project/subscriptions/both", "")
	if code != http.StatusOK || got["topic"] != topic {
		t.Errorf("REST read of a gRPC subscription = %d %v", code, got)
	}
}
