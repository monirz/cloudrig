package pubsub

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Subscriber implements pubsubpb.SubscriberServer.
type Subscriber struct {
	pubsubpb.UnimplementedSubscriberServer
	svc *Service
}

// NewSubscriber returns the subscriber half of the service.
func NewSubscriber(s *Service) *Subscriber { return &Subscriber{svc: s} }

func (b *Subscriber) CreateSubscription(ctx context.Context, sub *pubsubpb.Subscription) (*pubsubpb.Subscription, error) {
	if err := validName(sub.GetName(), "subscriptions"); err != nil {
		return nil, err
	}
	if err := validName(sub.GetTopic(), "topics"); err != nil {
		return nil, err
	}
	if _, err := b.svc.getTopic(ctx, sub.GetTopic()); err != nil {
		return nil, err
	}
	if sub.GetAckDeadlineSeconds() == 0 {
		sub.AckDeadlineSeconds = 10
	}

	encoded, err := json.Marshal(sub)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding subscription: %v", err)
	}
	if _, err := b.svc.kv.Put(ctx, subscriptionKey(sub.GetName()), encoded, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return nil, status.Errorf(codes.AlreadyExists, "Subscription already exists: %s", sub.GetName())
		}
		return nil, status.Errorf(codes.Internal, "storing subscription: %v", err)
	}
	return sub, nil
}

func (b *Subscriber) GetSubscription(ctx context.Context, req *pubsubpb.GetSubscriptionRequest) (*pubsubpb.Subscription, error) {
	return b.svc.getSubscription(ctx, req.GetSubscription())
}

func (b *Subscriber) ListSubscriptions(ctx context.Context, req *pubsubpb.ListSubscriptionsRequest) (*pubsubpb.ListSubscriptionsResponse, error) {
	project := strings.TrimPrefix(req.GetProject(), "projects/")
	entries, _, err := b.svc.kv.List(ctx, subscriptionPrefix+"projects/"+project+"/", 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing subscriptions: %v", err)
	}

	out := make([]*pubsubpb.Subscription, 0, len(entries))
	for _, kv := range entries {
		var sub pubsubpb.Subscription
		if err := json.Unmarshal(kv.Val, &sub); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding subscription: %v", err)
		}
		out = append(out, &sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &pubsubpb.ListSubscriptionsResponse{Subscriptions: out}, nil
}

func (b *Subscriber) DeleteSubscription(ctx context.Context, req *pubsubpb.DeleteSubscriptionRequest) (*emptypb.Empty, error) {
	name := req.GetSubscription()
	if err := b.svc.kv.Delete(ctx, subscriptionKey(name), 0); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "Subscription does not exist (resource=%s)", name)
		}
		return nil, status.Errorf(codes.Internal, "deleting subscription: %v", err)
	}

	b.svc.mu.Lock()
	delete(b.svc.backlog, name)
	delete(b.svc.outstanding, name)
	b.svc.mu.Unlock()
	return &emptypb.Empty{}, nil
}

// Pull takes messages off a subscription's backlog.
//
// returnImmediately is honoured rather than blocking: a pull with nothing to
// return answers empty. Blocking would need the wall clock, and StreamingPull
// is how a client waits for messages anyway.
func (b *Subscriber) Pull(ctx context.Context, req *pubsubpb.PullRequest) (*pubsubpb.PullResponse, error) {
	name := req.GetSubscription()
	if _, err := b.svc.getSubscription(ctx, name); err != nil {
		return nil, err
	}

	max := int(req.GetMaxMessages())
	if max <= 0 {
		max = 1
	}
	return &pubsubpb.PullResponse{ReceivedMessages: b.svc.take(name, max)}, nil
}

// take moves up to n messages from the backlog into outstanding.
func (s *Service) take(subscription string, n int) []*pubsubpb.ReceivedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue := s.backlog[subscription]
	if len(queue) == 0 {
		return nil
	}
	if n > len(queue) {
		n = len(queue)
	}
	taken, rest := queue[:n], queue[n:]
	s.backlog[subscription] = rest

	if s.outstanding[subscription] == nil {
		s.outstanding[subscription] = map[string]*pubsubpb.PubsubMessage{}
	}

	out := make([]*pubsubpb.ReceivedMessage, 0, len(taken))
	for _, msg := range taken {
		// The ack id is the message id: an emulator has no reason to make
		// them differ, and a readable ack id helps when a test prints one.
		ackID := msg.GetMessageId()
		s.outstanding[subscription][ackID] = msg
		out = append(out, &pubsubpb.ReceivedMessage{AckId: ackID, Message: msg})
	}
	return out
}

// Acknowledge drops messages the subscriber has handled.
func (b *Subscriber) Acknowledge(ctx context.Context, req *pubsubpb.AcknowledgeRequest) (*emptypb.Empty, error) {
	name := req.GetSubscription()
	if _, err := b.svc.getSubscription(ctx, name); err != nil {
		return nil, err
	}

	b.svc.mu.Lock()
	for _, id := range req.GetAckIds() {
		delete(b.svc.outstanding[name], id)
	}
	b.svc.mu.Unlock()
	return &emptypb.Empty{}, nil
}

// ModifyAckDeadline with a deadline of zero is a nack: the message goes back
// on the queue for redelivery. Any other deadline is accepted and ignored,
// since nothing here expires an outstanding message.
func (b *Subscriber) ModifyAckDeadline(ctx context.Context, req *pubsubpb.ModifyAckDeadlineRequest) (*emptypb.Empty, error) {
	name := req.GetSubscription()
	if _, err := b.svc.getSubscription(ctx, name); err != nil {
		return nil, err
	}
	if req.GetAckDeadlineSeconds() != 0 {
		return &emptypb.Empty{}, nil
	}

	b.svc.mu.Lock()
	for _, id := range req.GetAckIds() {
		if msg, ok := b.svc.outstanding[name][id]; ok {
			delete(b.svc.outstanding[name], id)
			// Returned to the front: a nacked message is the oldest thing
			// waiting, not the newest.
			b.svc.backlog[name] = append([]*pubsubpb.PubsubMessage{msg}, b.svc.backlog[name]...)
			b.svc.signal(name)
		}
	}
	b.svc.mu.Unlock()
	return &emptypb.Empty{}, nil
}
