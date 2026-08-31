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

// Publisher implements pubsubpb.PublisherServer.
type Publisher struct {
	pubsubpb.UnimplementedPublisherServer
	svc *Service
}

// NewPublisher returns the publisher half of the service.
func NewPublisher(s *Service) *Publisher { return &Publisher{svc: s} }

func (p *Publisher) CreateTopic(ctx context.Context, t *pubsubpb.Topic) (*pubsubpb.Topic, error) {
	if err := validName(t.GetName(), "topics"); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(t)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding topic: %v", err)
	}
	// ifVersion 0 makes the duplicate check and the write one step.
	if _, err := p.svc.kv.Put(ctx, topicKey(t.GetName()), encoded, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return nil, status.Errorf(codes.AlreadyExists, "Topic already exists: %s", t.GetName())
		}
		return nil, status.Errorf(codes.Internal, "storing topic: %v", err)
	}
	return t, nil
}

func (p *Publisher) GetTopic(ctx context.Context, req *pubsubpb.GetTopicRequest) (*pubsubpb.Topic, error) {
	return p.svc.getTopic(ctx, req.GetTopic())
}

func (p *Publisher) ListTopics(ctx context.Context, req *pubsubpb.ListTopicsRequest) (*pubsubpb.ListTopicsResponse, error) {
	project := strings.TrimPrefix(req.GetProject(), "projects/")
	entries, _, err := p.svc.kv.List(ctx, topicPrefix+"projects/"+project+"/", 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing topics: %v", err)
	}

	out := make([]*pubsubpb.Topic, 0, len(entries))
	for _, kv := range entries {
		var t pubsubpb.Topic
		if err := json.Unmarshal(kv.Val, &t); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding topic: %v", err)
		}
		out = append(out, &t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &pubsubpb.ListTopicsResponse{Topics: out}, nil
}

func (p *Publisher) DeleteTopic(ctx context.Context, req *pubsubpb.DeleteTopicRequest) (*emptypb.Empty, error) {
	if err := p.svc.kv.Delete(ctx, topicKey(req.GetTopic()), 0); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "Topic not found: %s", req.GetTopic())
		}
		return nil, status.Errorf(codes.Internal, "deleting topic: %v", err)
	}
	// Subscriptions outlive their topic, as in real Pub/Sub: they keep working
	// and simply receive nothing more.
	return &emptypb.Empty{}, nil
}

// Publish fans a message out to every subscription on the topic.
func (p *Publisher) Publish(ctx context.Context, req *pubsubpb.PublishRequest) (*pubsubpb.PublishResponse, error) {
	if _, err := p.svc.getTopic(ctx, req.GetTopic()); err != nil {
		return nil, err
	}

	subs, err := p.svc.subscriptionsFor(ctx, req.GetTopic())
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(req.GetMessages()))
	p.svc.mu.Lock()
	for _, msg := range req.GetMessages() {
		id := p.svc.nextMessageID()
		ids = append(ids, id)

		// Each subscription gets its own copy, so acknowledging on one does
		// not consume another's.
		for _, sub := range subs {
			delivered := &pubsubpb.PubsubMessage{
				Data:        msg.GetData(),
				Attributes:  msg.GetAttributes(),
				MessageId:   id,
				OrderingKey: msg.GetOrderingKey(),
				PublishTime: timestampOf(p.svc.clk.Now()),
			}
			p.svc.backlog[sub] = append(p.svc.backlog[sub], delivered)
			p.svc.signal(sub)
		}
	}
	p.svc.mu.Unlock()

	return &pubsubpb.PublishResponse{MessageIds: ids}, nil
}

// subscriptionsFor lists the subscriptions attached to a topic.
func (s *Service) subscriptionsFor(ctx context.Context, topic string) ([]string, error) {
	entries, _, err := s.kv.List(ctx, subscriptionPrefix+"projects/"+projectOf(topic)+"/", 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing subscriptions: %v", err)
	}

	var out []string
	for _, kv := range entries {
		var sub pubsubpb.Subscription
		if err := json.Unmarshal(kv.Val, &sub); err != nil {
			continue
		}
		if sub.GetTopic() == topic {
			out = append(out, sub.GetName())
		}
	}
	sort.Strings(out)
	return out, nil
}
