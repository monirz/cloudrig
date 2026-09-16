package pubsub

import (
	"context"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestAdminRPCs covers the topic and subscription admin surface the streaming
// path does not: listing, pulling, patching and deleting.
func TestAdminRPCs(t *testing.T) {
	t.Parallel()

	s := New(store.NewMemory(), clock.NewFake(epoch), nil)
	p := NewPublisher(s)
	b := NewSubscriber(s)
	ctx := context.Background()
	const topic = "projects/p/topics/t"
	const sub = "projects/p/subscriptions/s"

	if _, err := p.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if tl, err := p.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/p"}); err != nil {
		t.Fatalf("ListTopics: %v", err)
	} else if len(tl.GetTopics()) != 1 {
		t.Fatalf("ListTopics = %d, want 1", len(tl.GetTopics()))
	}

	if _, err := b.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if sl, err := b.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/p"}); err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	} else if len(sl.GetSubscriptions()) != 1 {
		t.Fatalf("ListSubscriptions = %d, want 1", len(sl.GetSubscriptions()))
	}

	if _, err := p.Publish(ctx, &pubsubpb.PublishRequest{
		Topic: topic, Messages: []*pubsubpb.PubsubMessage{{Data: []byte("hi")}},
	}); err != nil {
		t.Fatal(err)
	}
	pull, err := b.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(pull.GetReceivedMessages()) != 1 {
		t.Fatalf("Pull got %d messages, want 1", len(pull.GetReceivedMessages()))
	}

	upd, err := b.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sub, AckDeadlineSeconds: 30},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds"}},
	})
	if err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if upd.GetAckDeadlineSeconds() != 30 {
		t.Errorf("ack deadline = %d after update, want 30", upd.GetAckDeadlineSeconds())
	}

	if _, err := b.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: sub}); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
}
