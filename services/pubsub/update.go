package pubsub

import (
	"context"
	"encoding/json"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// UpdateTopic applies the fields named by the update mask. Terraform reaches
// this on every change to an existing topic.
func (p *Publisher) UpdateTopic(ctx context.Context, req *pubsubpb.UpdateTopicRequest) (*pubsubpb.Topic, error) {
	name := req.GetTopic().GetName()

	raw, version, err := p.svc.kv.Get(ctx, topicKey(name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Topic not found: %s", name)
	}
	var current pubsubpb.Topic
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding topic: %v", err)
	}

	if err := applyMask(&current, req.GetTopic(), req.GetUpdateMask(), "name"); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(&current)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding topic: %v", err)
	}
	// At the version just read, so a concurrent update cannot be lost.
	if _, err := p.svc.kv.Put(ctx, topicKey(name), encoded, version); err != nil {
		return nil, status.Errorf(codes.Aborted, "updating topic: %v", err)
	}
	return &current, nil
}

// UpdateSubscription applies the fields named by the update mask.
func (b *Subscriber) UpdateSubscription(ctx context.Context, req *pubsubpb.UpdateSubscriptionRequest) (*pubsubpb.Subscription, error) {
	name := req.GetSubscription().GetName()

	raw, version, err := b.svc.kv.Get(ctx, subscriptionKey(name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Subscription does not exist: %s", name)
	}
	var current pubsubpb.Subscription
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding subscription: %v", err)
	}

	// The topic is fixed at creation: a subscription that changed topics would
	// silently abandon its backlog.
	if err := applyMask(&current, req.GetSubscription(), req.GetUpdateMask(), "name", "topic"); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(&current)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding subscription: %v", err)
	}
	if _, err := b.svc.kv.Put(ctx, subscriptionKey(name), encoded, version); err != nil {
		return nil, status.Errorf(codes.Aborted, "updating subscription: %v", err)
	}
	return &current, nil
}

// applyMask copies the fields named by mask from src onto dst, refusing the
// ones that cannot change.
//
// Only the named fields move: a masked update that carried the whole resource
// would overwrite fields the caller never mentioned, which is the bug an
// update mask exists to prevent.
func applyMask(dst, src proto.Message, mask *fieldmaskpb.FieldMask, immutable ...string) error {
	paths := mask.GetPaths()
	if len(paths) == 0 {
		return status.Error(codes.InvalidArgument, "an update needs a non-empty update_mask")
	}

	d, s := dst.ProtoReflect(), src.ProtoReflect()
	fields := d.Descriptor().Fields()

	for _, path := range paths {
		for _, fixed := range immutable {
			if path == fixed {
				return status.Errorf(codes.InvalidArgument, "field %q cannot be updated", path)
			}
		}

		// A mask arrives in whichever spelling the caller's surface uses: the
		// REST API sends camelCase, gRPC sends the proto name.
		fd := fields.ByName(protoreflect.Name(path))
		if fd == nil {
			fd = fields.ByJSONName(path)
		}
		if fd == nil {
			return status.Errorf(codes.InvalidArgument, "unknown field %q in update_mask", path)
		}
		// An unset field in the request clears the stored one: that is what
		// naming it in the mask without a value means.
		if s.Has(fd) {
			d.Set(fd, s.Get(fd))
		} else {
			d.Clear(fd)
		}
	}
	return nil
}
