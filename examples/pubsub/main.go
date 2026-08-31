// Command pubsub-demo exercises the emulator's Pub/Sub the way an application
// would: nothing here names cloudrig, and the only configuration is
// PUBSUB_EMULATOR_HOST.
//
//	go run ./examples/pubsub -mode send
//	go run ./examples/pubsub -mode receive
//	go run ./examples/pubsub -mode crash    # dies holding a message
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func main() {
	mode := flag.String("mode", "send", "send, receive or crash")
	// The emulator's default, so the demo and `cloudrig fn` agree without
	// either being told.
	project := flag.String("project", "cloudrig-local", "project id")
	name := flag.String("topic", "orders", "topic name")
	body := flag.String("data", "order-42", "message body")
	wait := flag.Duration("wait", 25*time.Second, "how long to receive for")
	flag.Parse()

	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		log.Fatal("set PUBSUB_EMULATOR_HOST=localhost:4599")
	}

	ctx := context.Background()
	c, err := pubsub.NewClient(ctx, *project)
	if err != nil {
		log.Fatalf("client: %v", err)
	}
	defer c.Close()

	topic := fmt.Sprintf("projects/%s/topics/%s", *project, *name)
	sub := fmt.Sprintf("projects/%s/subscriptions/%s-worker", *project, *name)
	setup(ctx, c, topic, sub)

	switch *mode {
	case "send":
		send(ctx, c, topic, *body)
	case "receive":
		receive(ctx, c, sub, *wait)
	case "crash":
		crash(ctx, c, sub)
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

// setup creates the topic and subscription. A second run finds both already
// there, which is the expected case and not worth a line of output.
func setup(ctx context.Context, c *pubsub.Client, topic, sub string) {
	_, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic})
	report("topic", err)

	_, err = c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: sub, Topic: topic,
		// Short, so an unacknowledged message comes back while you watch.
		AckDeadlineSeconds: 10,
	})
	report("subscription", err)
}

// report prints anything except the AlreadyExists a repeated run produces.
func report(what string, err error) {
	if err != nil && status.Code(err) != codes.AlreadyExists {
		log.Printf("%s: %v", what, err)
	}
}

func send(ctx context.Context, c *pubsub.Client, topic, body string) {
	id, err := c.Publisher(topic).Publish(ctx, &pubsub.Message{
		Data:       []byte(body),
		Attributes: map[string]string{"source": "pubsub-demo"},
	}).Get(ctx)
	if err != nil {
		log.Fatalf("publish: %v", err)
	}
	fmt.Printf("published %s to %s\n", id, topic)
}

// receive prints and acknowledges every message.
func receive(ctx context.Context, c *pubsub.Client, sub string, wait time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	fmt.Printf("receiving from %s for %s\n", sub, wait)
	err := c.Subscriber(sub).Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		fmt.Printf("%s  id=%s  %s\n", time.Now().Format("15:04:05"), m.ID, m.Data)
		m.Ack()
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("receive: %v", err)
	}
}

// crash takes one message and dies still holding it, which is what an ack
// deadline is for: nobody nacks, so only the deadline can free the message.
//
// It exits hard rather than returning. A handler that neither acks nor nacks
// makes the client wait for it forever, so a graceful exit would never get
// here — and a worker that fails politely is not the case being tested.
func crash(ctx context.Context, c *pubsub.Client, sub string) {
	fmt.Printf("receiving from %s, then dying without an ack\n", sub)
	err := c.Subscriber(sub).Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		fmt.Printf("%s  id=%s  %s  <- taken, now crashing\n",
			time.Now().Format("15:04:05"), m.ID, m.Data)
		os.Exit(7)
	})
	if err != nil {
		log.Fatalf("receive: %v", err)
	}
}
