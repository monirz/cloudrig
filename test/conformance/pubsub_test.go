package conformance

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/functions"
	pubsub2 "github.com/monirz/cloudrig/services/pubsub"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// pubsubClient points the real client at an in-process emulator over gRPC.
//
// grpc.WithInsecure plus h2c on the emulator's side is what lets gRPC and REST
// share one port: the client connects with prior knowledge, and the transport
// dispatches on the content type.
func pubsubClient(t *testing.T) (*pubsub.Client, context.Context) {
	t.Helper()
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
	return c, ctx
}

func TestPubSubTopics(t *testing.T) {
	c, ctx := pubsubClient(t)
	admin := c.TopicAdminClient

	created, err := admin.CreateTopic(ctx, &pubsubpb.Topic{
		Name: "projects/test-project/topics/orders",
	})
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if created.GetName() != "projects/test-project/topics/orders" {
		t.Errorf("name = %q", created.GetName())
	}

	got, err := admin.GetTopic(ctx, &pubsubpb.GetTopicRequest{
		Topic: "projects/test-project/topics/orders",
	})
	if err != nil {
		t.Fatalf("GetTopic: %v", err)
	}
	if got.GetName() != created.GetName() {
		t.Errorf("read back %q", got.GetName())
	}
}

// TestPubSubPublishAndReceive is the whole point: a message published through
// the real client comes back to a real subscriber, over gRPC, on the same port
// the REST APIs use.
func TestPubSubPublishAndReceive(t *testing.T) {
	c, ctx := pubsubClient(t)

	const (
		topicName = "projects/test-project/topics/events"
		subName   = "projects/test-project/subscriptions/worker"
	)

	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: subName, Topic: topicName,
	}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	publisher := c.Publisher(topicName)
	defer publisher.Stop()

	id, err := publisher.Publish(ctx, &pubsub.Message{
		Data:       []byte("order-42"),
		Attributes: map[string]string{"kind": "order"},
	}).Get(ctx)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id == "" {
		t.Error("Publish returned no message id")
	}

	// Receive blocks until the context ends, so it is cancelled on the first
	// message rather than waited out.
	received, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var got *pubsub.Message
	err = c.Subscriber(subName).Receive(received, func(_ context.Context, m *pubsub.Message) {
		got = m
		m.Ack()
		cancel()
	})
	if err != nil && received.Err() == nil {
		t.Fatalf("Receive: %v", err)
	}

	if got == nil {
		t.Fatal("the subscriber received nothing")
	}
	if string(got.Data) != "order-42" {
		t.Errorf("data = %q, want order-42", got.Data)
	}
	if got.Attributes["kind"] != "order" {
		t.Errorf("attributes = %v", got.Attributes)
	}
}

// TestPubSubFanOut holds the rule that each subscription gets its own copy:
// acknowledging on one must not consume another's.
func TestPubSubFanOut(t *testing.T) {
	c, ctx := pubsubClient(t)
	const topicName = "projects/test-project/topics/fanout"

	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatal(err)
	}
	subs := []string{
		"projects/test-project/subscriptions/a",
		"projects/test-project/subscriptions/b",
	}
	for _, name := range subs {
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name: name, Topic: topicName,
		}); err != nil {
			t.Fatal(err)
		}
	}

	publisher := c.Publisher(topicName)
	defer publisher.Stop()
	if _, err := publisher.Publish(ctx, &pubsub.Message{Data: []byte("broadcast")}).Get(ctx); err != nil {
		t.Fatal(err)
	}

	for _, name := range subs {
		t.Run(name, func(t *testing.T) {
			received, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()

			var got string
			err := c.Subscriber(name).Receive(received, func(_ context.Context, m *pubsub.Message) {
				got = string(m.Data)
				m.Ack()
				cancel()
			})
			if err != nil && received.Err() == nil {
				t.Fatal(err)
			}
			if got != "broadcast" {
				t.Errorf("%s received %q, want broadcast", name, got)
			}
		})
	}
}

// TestPubSubNackRedelivers checks the other half of the ack contract.
func TestPubSubNackRedelivers(t *testing.T) {
	c, ctx := pubsubClient(t)
	const (
		topicName = "projects/test-project/topics/nacked"
		subName   = "projects/test-project/subscriptions/nacked"
	)

	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: subName, Topic: topicName,
	}); err != nil {
		t.Fatal(err)
	}

	publisher := c.Publisher(topicName)
	defer publisher.Stop()
	if _, err := publisher.Publish(ctx, &pubsub.Message{Data: []byte("retry me")}).Get(ctx); err != nil {
		t.Fatal(err)
	}

	received, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var deliveries int
	var mu sync.Mutex
	err := c.Subscriber(subName).Receive(received, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		deliveries++
		n := deliveries
		mu.Unlock()

		if n == 1 {
			m.Nack() // must come back
			return
		}
		m.Ack()
		cancel()
	})
	if err != nil && received.Err() == nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if deliveries < 2 {
		t.Errorf("delivered %d times, want the nacked message redelivered", deliveries)
	}
}

func TestPubSubErrors(t *testing.T) {
	c, ctx := pubsubClient(t)

	t.Run("duplicate topic is AlreadyExists", func(t *testing.T) {
		name := "projects/test-project/topics/dup"
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name}); err != nil {
			t.Fatal(err)
		}
		_, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name})
		if code := status.Code(err); code != codes.AlreadyExists {
			t.Errorf("code = %v, want AlreadyExists", code)
		}
	})

	t.Run("missing topic is NotFound", func(t *testing.T) {
		_, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{
			Topic: "projects/test-project/topics/ghost",
		})
		if code := status.Code(err); code != codes.NotFound {
			t.Errorf("code = %v, want NotFound", code)
		}
	})

	t.Run("a subscription on a missing topic is NotFound", func(t *testing.T) {
		_, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
			Name:  "projects/test-project/subscriptions/orphan",
			Topic: "projects/test-project/topics/ghost",
		})
		if code := status.Code(err); code != codes.NotFound {
			t.Errorf("code = %v, want NotFound", code)
		}
	})
}

// TestPublishFiresAFunction is the pair to TestUploadFiresAFunction: a message
// published through the real client runs a function, in one process, with no
// Docker and no push endpoint.
func TestPublishFiresAFunction(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	if _, err := emu.Functions().Deploy(ctx, functions.Function{
		Name:   "on-message",
		Source: "../../testdata/go-pubsub",
		Trigger: functions.EventTrigger{
			EventType: pubsub2.EventPublish,
			Resource:  "orders",
		},
	}); err != nil {
		t.Fatal(err)
	}

	c, err := pubsub.NewClient(ctx, "test-project",
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	const topic = "projects/test-project/topics/orders"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	res := c.Publisher(topic).Publish(ctx, &pubsub.Message{
		Data:       []byte("order-42"),
		Attributes: map[string]string{"region": "eu"},
	})
	if _, err := res.Get(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Delivery is asynchronous so the publish never waits on it.
	emu.SyncEvents()

	inst, ok := emu.Functions().Instance("", "", "on-message")
	if !ok {
		t.Fatal("the function is not deployed")
	}
	logged := strings.Join(inst.LogSnapshot(), "\n")
	for _, want := range []string{
		"FIRED",
		pubsub2.EventPublish,
		`"order-42"`,
		topic,
		"pubsub.googleapis.com",
		"region:eu",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the function log is missing %q:\n%s", want, logged)
		}
	}
}

// TestPublishToAnotherTopicDoesNotFire holds the other half: a trigger scoped
// to one topic must not run for another.
func TestPublishToAnotherTopicDoesNotFire(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	if _, err := emu.Functions().Deploy(ctx, functions.Function{
		Name:    "scoped",
		Source:  "../../testdata/go-pubsub",
		Trigger: functions.EventTrigger{EventType: pubsub2.EventPublish, Resource: "wanted"},
	}); err != nil {
		t.Fatal(err)
	}

	c, err := pubsub.NewClient(ctx, "test-project",
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	const topic = "projects/test-project/topics/unwanted"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	res := c.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("x")})
	if _, err := res.Get(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	emu.SyncEvents()

	inst, _ := emu.Functions().Instance("", "", "scoped")
	if logged := strings.Join(inst.LogSnapshot(), "\n"); strings.Contains(logged, "FIRED") {
		t.Errorf("a trigger scoped to one topic ran for another:\n%s", logged)
	}
}

// TestPubSubEmulatorHost is the promise the env var makes: code written
// against real Pub/Sub finds the emulator with no client options at all.
//
// Not parallel: it sets an environment variable, which is process-wide.
func TestPubSubEmulatorHost(t *testing.T) {
	emu := cloudrig.MustStart(t)
	t.Setenv("PUBSUB_EMULATOR_HOST", emu.Endpoint())

	ctx := context.Background()
	c, err := pubsub.NewClient(ctx, "test-project")
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	const topic = "projects/test-project/topics/env"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if _, err := c.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("x")}).Get(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}
