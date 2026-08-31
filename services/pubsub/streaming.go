package pubsub

import (
	"io"

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
	if _, err := b.svc.getSubscription(ctx, name); err != nil {
		return err
	}
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

	wait := b.svc.waitCh(name)
	for {
		if messages := b.svc.take(name, streamBatch); len(messages) > 0 {
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
		delete(b.svc.outstanding[subscription], id)
	}

	// A modify-deadline of zero is a nack: the message goes back to the front
	// of the queue, since it is the oldest thing still waiting.
	for i, id := range req.GetModifyDeadlineAckIds() {
		if i >= len(req.GetModifyDeadlineSeconds()) || req.GetModifyDeadlineSeconds()[i] != 0 {
			continue
		}
		if msg, ok := b.svc.outstanding[subscription][id]; ok {
			delete(b.svc.outstanding[subscription], id)
			b.svc.backlog[subscription] = append([]*pubsubpb.PubsubMessage{msg}, b.svc.backlog[subscription]...)
			b.svc.signal(subscription)
		}
	}
}
