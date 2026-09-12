package conformance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"strings"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/core/faults"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestFaultFailsARequest is the base case: a rule turns a call that would have
// succeeded into the error the test asked for.
func TestFaultFailsARequest(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	emu.Faults().Add(faults.Rule{
		Path:    "/storage/v1/*",
		Status:  http.StatusTooManyRequests,
		Message: "slow down",
	})

	resp, err := http.Get(emu.BaseURL() + "/storage/v1/b/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "slow down") {
		t.Errorf("body = %s, want the rule's message", body)
	}
}

// TestFaultLetsTheRetryThrough is the reason the package exists: the real
// client retries a 503, and a rule with a count of 1 proves the retry happened
// and succeeded.
func TestFaultLetsTheRetryThrough(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := storage.NewClient(ctx,
		option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Bucket("retry").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}

	// One failure, then the truth. A client that does not retry fails here.
	emu.Faults().Add(faults.Rule{Path: "/storage/v1/*", Count: 1})

	if _, err := c.Bucket("retry").Attrs(ctx); err != nil {
		t.Fatalf("the client did not retry through a single 503: %v", err)
	}
	if n := emu.Faults().Len(); n != 0 {
		t.Errorf("%d rules still armed, want the spent one dropped", n)
	}
}

// TestFaultSurfacesToTheClient holds that an injected error reaches the caller
// as a real API error, not as a transport failure.
func TestFaultSurfacesToTheClient(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := storage.NewClient(ctx,
		option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	emu.Faults().Add(faults.Rule{
		Path:    "/storage/v1/*",
		Status:  http.StatusForbidden,
		Message: "no",
	})

	_, err = c.Bucket("denied").Attrs(ctx)
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want a *googleapi.Error", err, err)
	}
	if apiErr.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", apiErr.Code)
	}
}

// TestFaultsSpareTheAdminAPI is what keeps a broad rule from locking a test
// out of its own controls.
func TestFaultsSpareTheAdminAPI(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	emu.Faults().Add(faults.Rule{}) // everything

	resp, err := http.Get(emu.BaseURL() + "/_emu/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health = %d under a match-everything rule, want 200", resp.StatusCode)
	}
}

// TestFaultHitsGRPC is the cross-protocol case: the same fault Set that fails
// REST also fails a unary gRPC call, mapped to a real gRPC status code (not an
// HTTP number). This is what makes `cloudrig fault pubsub` reach a real client.
func TestFaultHitsGRPC(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := pubsub.NewClient(ctx, "test-project",
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	admin := c.TopicAdminClient

	// Without a fault, the gRPC call succeeds.
	if _, err := admin.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/test-project/topics/ok"}); err != nil {
		t.Fatalf("CreateTopic before fault: %v", err)
	}

	// A fault on the Pub/Sub gRPC prefix, asked for as HTTP 500.
	emu.Faults().Add(faults.Rule{Path: "/google.pubsub.v1.*", Status: 500})

	_, err = admin.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/test-project/topics/faulted"})
	if got := status.Code(err); got != codes.Internal {
		t.Errorf("gRPC fault code = %v, want Internal (500 mapped to a gRPC code, not smuggled)", got)
	}

	// Cleared, the gRPC call works again.
	emu.Faults().Clear()
	if _, err := admin.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/test-project/topics/after"}); err != nil {
		t.Errorf("CreateTopic after clear: %v", err)
	}
}
