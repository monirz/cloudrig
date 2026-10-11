package pubsub

import (
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
