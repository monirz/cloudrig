package conformance

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/monirz/cloudrig"
)

const (
	pendingTopic = "projects/test-project/topics/orders"
	pendingSub   = "projects/test-project/subscriptions/audit"
)

func pubsubFor(t *testing.T, emu *cloudrig.Emulator) *pubsub.Client {
	t.Helper()
	c, err := pubsub.NewClient(context.Background(), "test-project",
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// withPending returns an emulator holding two published, unreceived messages.
func withPending(t *testing.T) *cloudrig.Emulator {
	t.Helper()
	emu := cloudrig.MustStart(t)
	c := pubsubFor(t, emu)
	ctx := context.Background()
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: pendingTopic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: pendingSub, Topic: pendingTopic}); err != nil {
		t.Fatal(err)
	}
	pub := c.Publisher(pendingTopic)
	defer pub.Stop()
	for _, body := range []string{"order-1", "order-2"} {
		if _, err := pub.Publish(ctx, &pubsub.Message{Data: []byte(body)}).Get(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return emu
}

// receive collects n messages from the subscription, acknowledging each.
func receive(t *testing.T, emu *cloudrig.Emulator, n int) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var (
		mu  sync.Mutex
		got []string
	)
	err := pubsubFor(t, emu).Subscriber(pendingSub).Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, string(m.Data))
		m.Ack()
		if len(got) == n {
			cancel()
		}
	})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("Receive: %v", err)
	}
	slices.Sort(got)
	return got
}

func TestSnapshotCarriesPendingMessages(t *testing.T) {
	t.Parallel()
	src := withPending(t)

	resp, err := http.Get(src.BaseURL() + "/_emu/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	dst := cloudrig.MustStart(t)
	post, err := http.Post(dst.BaseURL()+"/_emu/snapshot", "application/x-tar", bytes.NewReader(snap))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusNoContent {
		t.Fatalf("restore POST = %d, want 204", post.StatusCode)
	}

	if got := receive(t, dst, 2); !slices.Equal(got, []string{"order-1", "order-2"}) {
		t.Errorf("restored emulator delivered %v, want order-1 and order-2", got)
	}
}

func TestForkCarriesPendingMessages(t *testing.T) {
	t.Parallel()
	src := withPending(t)
	fork := src.Fork(t)

	want := []string{"order-1", "order-2"}
	if got := receive(t, fork, 2); !slices.Equal(got, want) {
		t.Errorf("fork delivered %v, want %v", got, want)
	}
	// Receiving in the fork must not consume the parent's copies.
	if got := receive(t, src, 2); !slices.Equal(got, want) {
		t.Errorf("parent delivered %v after the fork received, want %v", got, want)
	}
}
