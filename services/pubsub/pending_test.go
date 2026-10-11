package pubsub

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

func TestPendingRoundTrips(t *testing.T) {
	t.Parallel()

	s := newService(t)
	const sub = "projects/p/subscriptions/s"
	s.mu.Lock()
	for _, body := range []string{"a", "b", "c"} {
		s.backlog[sub] = append(s.backlog[sub], &pubsubpb.PubsubMessage{
			MessageId: s.nextMessageID(), Data: []byte(body),
		})
	}
	s.mu.Unlock()
	s.take(sub, 1, time.Minute) // "a" is leased, not acked: still pending

	seq, msgs := s.Pending()
	if seq != 3 {
		t.Errorf("seq = %d, want 3", seq)
	}

	r := newService(t)
	subscribe(t, r, sub)
	r.RestorePending(seq, msgs)
	got := r.take(sub, 10, time.Minute)
	var bodies string
	for _, m := range got {
		bodies += string(m.GetMessage().GetData())
	}
	if bodies != "abc" {
		t.Errorf("restored delivery = %q, want abc: the leased message first", bodies)
	}
	if id := r.nextMessageID(); id != "4" {
		t.Errorf("next id after restore = %s, want 4: ids must not repeat", id)
	}
}

func TestRestorePendingReplaces(t *testing.T) {
	t.Parallel()

	s := newService(t)
	const sub = "projects/p/subscriptions/s"
	s.backlog[sub] = []*pubsubpb.PubsubMessage{{MessageId: "1", Data: []byte("old")}}
	s.take(sub, 1, time.Minute)

	s.RestorePending(0, nil)
	if seq, msgs := s.Pending(); seq != 0 || len(msgs) != 0 {
		t.Errorf("after restoring nothing: seq %d, %d subscriptions pending", seq, len(msgs))
	}
}

// An ack held from before a restore must not settle a delivery made after it,
// even of a message with the same id.
func TestStaleAckAfterRestore(t *testing.T) {
	t.Parallel()

	s := newService(t)
	const sub = "projects/p/subscriptions/s"
	subscribe(t, s, sub)
	s.backlog[sub] = []*pubsubpb.PubsubMessage{{MessageId: "1", Data: []byte("a")}}
	seq, msgs := s.Pending()
	stale := s.take(sub, 1, time.Minute)[0].GetAckId()

	s.RestorePending(seq, msgs)
	fresh := s.take(sub, 1, time.Minute)[0].GetAckId()
	if fresh == stale {
		t.Fatalf("ack id %q reused across a restore", fresh)
	}

	if _, err := NewSubscriber(s).Acknowledge(context.Background(), &pubsubpb.AcknowledgeRequest{
		Subscription: sub, AckIds: []string{stale},
	}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.outstanding[sub]) != 1 {
		t.Errorf("a stale ack settled the new delivery: outstanding = %d", len(s.outstanding[sub]))
	}
}

// A message whose subscription is not in the store was captured while the
// subscription was being deleted; restoring it would leave an orphan.
func TestRestorePendingDropsOrphans(t *testing.T) {
	t.Parallel()

	s := newService(t)
	const kept, gone = "projects/p/subscriptions/kept", "projects/p/subscriptions/gone"
	subscribe(t, s, kept)
	s.RestorePending(2, map[string][]*pubsubpb.PubsubMessage{
		kept: {{MessageId: "1"}},
		gone: {{MessageId: "2"}},
	})

	_, msgs := s.Pending()
	if len(msgs[kept]) != 1 || len(msgs[gone]) != 0 {
		t.Errorf("pending = %v, want only the kept subscription's message", msgs)
	}
}
