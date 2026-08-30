package storage

import (
	"context"

	"github.com/monirz/cloudrig/core/events"
)

// The event types GCS emits to first-generation functions.
//
// These are the gen1 names, not the CloudEvents ones
// (google.cloud.storage.object.v1.finalized). We serve the v1 API and report
// GEN_1, so a function receives what a gen1 function receives — the names and
// the envelope have to agree, or a handler reads undefined.
const (
	EventFinalized = "google.storage.object.finalize"
	EventDeleted   = "google.storage.object.delete"
	EventArchived  = "google.storage.object.archive"
	EventUpdated   = "google.storage.object.metadataUpdate"
)

// ServiceName is what gen1 reports in an event's resource.service.
const ServiceName = "storage.googleapis.com"

// SourceOf identifies a bucket as the source of an event.
func SourceOf(bucket string) string {
	return "//storage.googleapis.com/projects/_/buckets/" + bucket
}

// resourceName is what gen1 puts in context.resource.name.
func resourceName(o Object) string {
	return "projects/_/buckets/" + o.Bucket + "/objects/" + o.Name + "#" + itoa(o.Generation)
}

// notify publishes an object event, if anything is listening.
//
// It is called after the change is durable, never before: a subscriber that
// reads the object must find it there.
func (s *Service) notify(ctx context.Context, kind string, o Object) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(ctx, events.Event{
		Type:     kind,
		Source:   SourceOf(o.Bucket),
		Subject:  "objects/" + o.Name,
		Resource: resourceName(o),
		Service:  ServiceName,
		Kind:     "storage#object",
		Time:     s.clk.Now(),
		Data:     objectPayload(o),
	})
}

// objectPayload is the object resource GCS puts in the event body. It is the
// same shape the JSON API returns, so a handler can decode it with the client
// library's object type.
func objectPayload(o Object) map[string]any {
	data := map[string]any{
		"kind":           "storage#object",
		"id":             o.Bucket + "/" + o.Name + "/" + itoa(o.Generation),
		"bucket":         o.Bucket,
		"name":           o.Name,
		"generation":     itoa(o.Generation),
		"metageneration": itoa(o.Metageneration),
		"size":           itoa(o.Size),
		"contentType":    o.ContentType,
		"crc32c":         o.CRC32C,
		"md5Hash":        o.MD5,
		"etag":           o.ETag,
		"timeCreated":    rfc3339(o.Created),
		"updated":        rfc3339(o.Updated),
	}
	if len(o.Metadata) > 0 {
		data["metadata"] = o.Metadata
	}
	return data
}
