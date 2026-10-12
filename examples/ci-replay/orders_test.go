package orders

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const project = "cloudrig-local"

// TestEveryOrderIsAnnounced places ten orders, then checks that each one saved
// in the bucket also reached the topic. Against a healthy emulator it passes;
// with a slow Publish armed it fails the same way every run.
func TestEveryOrderIsAnnounced(t *testing.T) {
	if os.Getenv("STORAGE_EMULATOR_HOST") == "" || os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("set STORAGE_EMULATOR_HOST and PUBSUB_EMULATOR_HOST to a running cloudrig")
	}
	ctx := context.Background()

	sc, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	bucket := sc.Bucket("orders")
	if err := bucket.Create(ctx, project, nil); err != nil && !exists(err) {
		t.Fatal(err)
	}

	pc, err := pubsub.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	topic := "projects/" + project + "/topics/orders"
	sub := "projects/" + project + "/subscriptions/orders-audit"
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil && !exists(err) {
		t.Fatal(err)
	}
	_, err = pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic})
	if err != nil && !exists(err) {
		t.Fatal(err)
	}

	pub := pc.Publisher(topic)
	pub.PublishSettings.Timeout = 2 * time.Second // the service's publish budget
	defer pub.Stop()
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("order-%d", i)
		if err := Place(ctx, bucket, pub, id); err != nil {
			t.Logf("place: %v", err)
		}
	}

	saved := savedOrders(ctx, t, bucket)
	announced := announcedOrders(ctx, t, pc.Subscriber(sub))
	for _, id := range saved {
		if !slices.Contains(announced, id) {
			t.Errorf("%s is saved but was never announced", id)
		}
	}
}

func savedOrders(ctx context.Context, t *testing.T, bucket *storage.BucketHandle) []string {
	var ids []string
	it := bucket.Objects(ctx, nil)
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return ids
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.Name[:len(a.Name)-len(".json")])
	}
}

// announcedOrders drains the subscription until it has been quiet briefly.
func announcedOrders(ctx context.Context, t *testing.T, s *pubsub.Subscriber) []string {
	var (
		mu  sync.Mutex
		ids []string
	)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := s.Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		ids = append(ids, string(m.Data))
		mu.Unlock()
		m.Ack()
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	return ids
}

func exists(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == 409 {
		return true
	}
	return status.Code(err) == codes.AlreadyExists
}
