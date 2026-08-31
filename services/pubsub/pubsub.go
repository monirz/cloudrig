// Package pubsub is the Cloud Pub/Sub emulation.
//
// gRPC, not REST: the Go client speaks only gRPC, so a REST-only emulation
// would be unreachable from the client anyone actually uses. This is where the
// grpc dependency finally earns its place — the transport has carried an h2c
// dispatch point since the first milestone, answering 501, waiting for a
// service that exercises it.
package pubsub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/events"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service holds topics, subscriptions and undelivered messages.
type Service struct {
	kv  store.Store
	clk clock.Clock
	bus *events.Bus

	mu sync.Mutex
	// backlog holds messages a subscription has not acknowledged, keyed by
	// subscription name. Messages are small and a local emulator holds few, so
	// they live in memory rather than in the blob tree.
	backlog map[string][]*pubsubpb.PubsubMessage
	// outstanding holds messages delivered but not yet acknowledged, so a nack
	// or an expired deadline can return them.
	outstanding map[string]map[string]*lease
	// waiters wakes streaming pulls when a message arrives: one channel per
	// live stream, never one shared by all of them. A stream waits on a signal
	// rather than polling, so nothing here needs a wall clock and a fake one
	// cannot stall delivery.
	waiters map[string]map[chan struct{}]struct{}
	seq     uint64
}

// New wires a service.
func New(kv store.Store, clk clock.Clock, bus *events.Bus) *Service {
	return &Service{
		kv:          kv,
		clk:         clk,
		bus:         bus,
		backlog:     map[string][]*pubsubpb.PubsubMessage{},
		outstanding: map[string]map[string]*lease{},
		waiters:     map[string]map[chan struct{}]struct{}{},
	}
}

// lease is a delivered message and the timer that takes it back. A subscriber
// that never acks must not hold a message forever: real Pub/Sub redelivers
// once the deadline passes, and a test that relies on that would otherwise
// pass here and fail in production.
type lease struct {
	msg   *pubsubpb.PubsubMessage
	timer clock.Timer
}

// redeliver returns a leased message to the front of the queue. The caller
// holds the lock.
func (s *Service) redeliver(subscription, ackID string) {
	l, ok := s.outstanding[subscription][ackID]
	if !ok {
		return
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	delete(s.outstanding[subscription], ackID)

	// To the front: a returned message is the oldest thing waiting, not the
	// newest.
	s.backlog[subscription] = append([]*pubsubpb.PubsubMessage{l.msg}, s.backlog[subscription]...)
	s.signal(subscription)
}

// expire is what a lapsed deadline runs. It re-checks under the lock, because
// an ack may have arrived while the timer was firing.
func (s *Service) expire(subscription, ackID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redeliver(subscription, ackID)
}

// Key layout. Topics and subscriptions are addressed by their resource names,
// which already carry the project, so they need no further prefixing.
func topicKey(name string) string        { return "ps/t/" + name }
func subscriptionKey(name string) string { return "ps/s/" + name }

const (
	topicPrefix        = "ps/t/"
	subscriptionPrefix = "ps/s/"
)

// validTopicName checks the resource name shape the API requires.
func validName(name, kind string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != kind {
		return status.Errorf(codes.InvalidArgument,
			"invalid resource name %q; expected projects/{project}/%s/{id}", name, kind)
	}
	if parts[1] == "" || parts[3] == "" {
		return status.Errorf(codes.InvalidArgument, "invalid resource name %q", name)
	}
	return nil
}

// projectOf pulls the project out of a resource name.
func projectOf(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func (s *Service) getTopic(ctx context.Context, name string) (*pubsubpb.Topic, error) {
	raw, _, err := s.kv.Get(ctx, topicKey(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "Topic not found: %s", name)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading topic: %v", err)
	}

	var t pubsubpb.Topic
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding topic: %v", err)
	}
	return &t, nil
}

func (s *Service) getSubscription(ctx context.Context, name string) (*pubsubpb.Subscription, error) {
	raw, _, err := s.kv.Get(ctx, subscriptionKey(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "Subscription does not exist (resource=%s)", name)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading subscription: %v", err)
	}

	var sub pubsubpb.Subscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding subscription: %v", err)
	}
	return &sub, nil
}

// signal wakes anything waiting on a subscription. The caller holds the lock.
//
// The channel is buffered and the send non-blocking: a signal means "look
// again", so one pending is as good as many, and a publish must never wait on
// a subscriber.
func (s *Service) signal(subscription string) {
	for ch := range s.waiters[subscription] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// listen registers a stream's own wake-up channel and returns it with the
// function that removes it.
//
// One channel per stream, because a shared one loses wake-ups: a stream on its
// way out can take the token meant for the stream that is staying, which then
// sleeps through a message already sitting in the backlog.
func (s *Service) listen(subscription string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	s.mu.Lock()
	if s.waiters[subscription] == nil {
		s.waiters[subscription] = map[chan struct{}]struct{}{}
	}
	s.waiters[subscription][ch] = struct{}{}
	s.mu.Unlock()

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.waiters[subscription], ch)
		if len(s.waiters[subscription]) == 0 {
			delete(s.waiters, subscription)
		}
	}
}

// nextMessageID hands out ids that are unique and increasing, so a test can
// tell the order messages were published in.
func (s *Service) nextMessageID() string {
	s.seq++
	return fmt.Sprintf("%d", s.seq)
}
