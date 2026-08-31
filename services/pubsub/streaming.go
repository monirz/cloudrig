package pubsub

import (
	"io"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// streamBatch is how many messages one send carries.
const streamBatch = 100

// StreamingPull is how the Go client receives.
//
// Subscriber.Receive opens a bidirectional stream and never calls Pull, so a
// service implementing only Pull is unreachable from the client anyone uses.
// The stream carries acks and nacks upward and messages downward at once.
func (b *Subscriber) StreamingPull(stream pubsubpb.Subscriber_StreamingPullServer) error {
	// The first request names the subscription; later ones carry acks.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	name := first.GetSubscription()
	if name == "" {
		return status.Error(codes.InvalidArgument, "the first StreamingPull request must name a subscription")
	}
	ctx := stream.Context()
	sub, err := b.svc.getSubscription(ctx, name)
	if err != nil {
		return err
	}
	deadline := deadlineOf(sub)
	b.handleAcks(name, first)

	// Acks arrive on their own goroutine so a quiet subscription still
	// processes them, and a busy one still sends.
	recvErr := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			b.handleAcks(name, req)
		}
	}()

	wait, stopListening := b.svc.listen(name)
	defer stopListening()

	for {
		if messages := b.svc.take(name, streamBatch, deadline); len(messages) > 0 {
			if err := stream.Send(&pubsubpb.StreamingPullResponse{ReceivedMessages: messages}); err != nil {
				return err
			}
			continue
		}

		select {
		case <-wait:
			// Something arrived; go round and take it.
		case err := <-recvErr:
			// The client closing its half is an ordinary end of stream.
			if err == io.EOF {
				return nil
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// handleAcks applies the acknowledgements and nacks a request carries.
func (b *Subscriber) handleAcks(subscription string, req *pubsubpb.StreamingPullRequest) {
	b.svc.mu.Lock()
	defer b.svc.mu.Unlock()

	for _, id := range req.GetAckIds() {
		if l, ok := b.svc.outstanding[subscription][id]; ok {
			l.timer.Stop()
			delete(b.svc.outstanding[subscription], id)
		}
	}

	// A modify-deadline of zero is a nack; anything else restarts the lease,
	// which is how the client keeps a message it is still working on.
	for i, id := range req.GetModifyDeadlineAckIds() {
		if i >= len(req.GetModifyDeadlineSeconds()) {
			continue
		}
		secs := req.GetModifyDeadlineSeconds()[i]
		if secs == 0 {
			b.svc.redeliver(subscription, id)
			continue
		}
		l, ok := b.svc.outstanding[subscription][id]
		if !ok {
			continue
		}
		// A timer that has already fired cannot be stopped, and its message is
		// back on the queue: extending it would lease a message twice.
		if l.timer != nil && !l.timer.Stop() {
			continue
		}
		l.timer = b.svc.clk.AfterFunc(time.Duration(secs)*time.Second,
			func() { b.svc.expire(subscription, id) })
	}
}
