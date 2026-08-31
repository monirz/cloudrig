package pubsub

import (
	"context"
	"encoding/base64"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig/core/events"
)

// EventPublish is the event type a gen1 function is triggered by. Pub/Sub has
// only the one: a message arrived.
const EventPublish = "google.pubsub.topic.publish"

// ServiceName is what gen1 reports in an event's resource.service.
const ServiceName = "pubsub.googleapis.com"

// MessageType is the proto name gen1 puts in resource.type and in the payload.
const MessageType = "type.googleapis.com/google.pubsub.v1.PubsubMessage"

// SourceOf identifies a topic as the source of an event.
func SourceOf(topic string) string { return "//pubsub.googleapis.com/" + topic }

// notify tells the bus a message was published, if anything is listening.
//
// The trigger fires on the publish itself, not on a subscription: a function
// deployed against a topic sees every message, whether or not anyone has
// subscribed.
func (s *Service) notify(ctx context.Context, topic string, msg *pubsubpb.PubsubMessage) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(ctx, events.Event{
		Type:     EventPublish,
		Source:   SourceOf(topic),
		Subject:  "topics/" + topic,
		Resource: topic,
		Service:  ServiceName,
		Kind:     MessageType,
		Time:     msg.GetPublishTime().AsTime(),
		Data:     messagePayload(msg),
	})
}

// messagePayload is the PubsubMessage as gen1 delivers it: the body is the
// message itself, with data base64-encoded the way the wire carries it.
func messagePayload(msg *pubsubpb.PubsubMessage) map[string]any {
	data := map[string]any{
		"@type":       MessageType,
		"data":        base64.StdEncoding.EncodeToString(msg.GetData()),
		"messageId":   msg.GetMessageId(),
		"publishTime": msg.GetPublishTime().AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z"),
	}
	if len(msg.GetAttributes()) > 0 {
		data["attributes"] = msg.GetAttributes()
	}
	return data
}
