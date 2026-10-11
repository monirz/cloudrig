package pubsub

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strconv"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
)

// Pending returns every unacknowledged message by subscription, leased ones
// first, plus the message-id counter, so a snapshot or fork carries them.
func (s *Service) Pending() (uint64, map[string][]*pubsubpb.PubsubMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := map[string][]*pubsubpb.PubsubMessage{}
	for sub, leases := range s.outstanding {
		ls := slices.Collect(maps.Values(leases))
		slices.SortFunc(ls, func(a, b *lease) int { return byID(a.msg, b.msg) })
		for _, l := range ls {
			out[sub] = append(out[sub], proto.CloneOf(l.msg))
		}
	}
	for sub, queue := range s.backlog {
		for _, m := range queue {
			out[sub] = append(out[sub], proto.CloneOf(m))
		}
	}
	for sub, msgs := range out {
		if len(msgs) == 0 {
			delete(out, sub)
		}
	}
	return s.seq, out
}

// RestorePending replaces every pending message and the id counter. Leases
// are dropped: a restored message is delivered afresh. Messages are captured
// before the store, so one whose subscription the store lacks was deleted in
// between, and is dropped with it.
func (s *Service) RestorePending(seq uint64, msgs map[string][]*pubsubpb.PubsubMessage) {
	live := map[string]bool{}
	for sub := range msgs {
		_, _, err := s.kv.Get(context.Background(), subscriptionKey(sub))
		live[sub] = err == nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, leases := range s.outstanding {
		for _, l := range leases {
			l.timer.Stop()
		}
	}
	s.outstanding = map[string]map[string]*lease{}
	s.backlog = map[string][]*pubsubpb.PubsubMessage{}
	for sub, queue := range msgs {
		if !live[sub] {
			continue
		}
		for _, m := range queue {
			s.backlog[sub] = append(s.backlog[sub], proto.CloneOf(m))
		}
		s.signal(sub)
	}
	s.seq = seq
}

// byID orders messages by the numeric id nextMessageID gives them.
func byID(a, b *pubsubpb.PubsubMessage) int {
	x, errX := strconv.ParseUint(a.GetMessageId(), 10, 64)
	y, errY := strconv.ParseUint(b.GetMessageId(), 10, 64)
	if errX != nil || errY != nil {
		return cmp.Compare(a.GetMessageId(), b.GetMessageId())
	}
	return cmp.Compare(x, y)
}
