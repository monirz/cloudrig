package functions

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/events"
)

func sample() events.Event {
	return events.Event{
		Type:     "google.storage.object.finalize",
		Source:   "//storage.googleapis.com/projects/_/buckets/uploads",
		Subject:  "objects/report.csv",
		Resource: "projects/_/buckets/uploads/objects/report.csv#17",
		Service:  "storage.googleapis.com",
		Kind:     "storage#object",
		Time:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Data:     map[string]any{"bucket": "uploads", "name": "report.csv"},
	}
}

// TestBackgroundEventShape pins the first-generation envelope.
//
// functions-framework reads body.data and body.context, and when there is no
// context it treats every other field as one. A CloudEvent body therefore
// "works" while giving a handler specversion and datacontenttype as its
// context, and no eventType at all — which is why this is asserted field by
// field rather than by round-tripping our own type.
func TestBackgroundEventShape(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(backgroundEvent(sample()))
	if err != nil {
		t.Fatal(err)
	}

	var body struct {
		Data    map[string]any `json:"data"`
		Context struct {
			EventID   string `json:"eventId"`
			Timestamp string `json:"timestamp"`
			EventType string `json:"eventType"`
			Resource  struct {
				Name    string `json:"name"`
				Service string `json:"service"`
				Type    string `json:"type"`
			} `json:"resource"`
		} `json:"context"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}

	if body.Data["name"] != "report.csv" {
		t.Errorf("data = %v", body.Data)
	}
	if body.Context.EventType != "google.storage.object.finalize" {
		t.Errorf("context.eventType = %q", body.Context.EventType)
	}
	if body.Context.Resource.Name != "projects/_/buckets/uploads/objects/report.csv#17" {
		t.Errorf("context.resource.name = %q", body.Context.Resource.Name)
	}
	if body.Context.Resource.Service != "storage.googleapis.com" {
		t.Errorf("context.resource.service = %q", body.Context.Resource.Service)
	}
	if body.Context.Resource.Type != "storage#object" {
		t.Errorf("context.resource.type = %q", body.Context.Resource.Type)
	}
	if body.Context.Timestamp != "2026-01-01T00:00:00Z" {
		t.Errorf("context.timestamp = %q", body.Context.Timestamp)
	}
	if body.Context.EventID == "" {
		t.Error("context.eventId is empty")
	}

	// A CloudEvents body would carry these, and their absence is what keeps a
	// gen1 handler from mistaking them for its context.
	var raw map[string]any
	_ = json.Unmarshal(encoded, &raw)
	for _, field := range []string{"specversion", "type", "source", "datacontenttype"} {
		if _, present := raw[field]; present {
			t.Errorf("the envelope carries the CloudEvents field %q", field)
		}
	}
}

// TestEventIDIsStable holds the retry contract: a repeated delivery must carry
// the id of the delivery it repeats, not look like a new event.
func TestEventIDIsStable(t *testing.T) {
	t.Parallel()

	if eventID(sample()) != eventID(sample()) {
		t.Error("the same event produced two ids")
	}

	other := sample()
	other.Resource += "8"
	if eventID(sample()) == eventID(other) {
		t.Error("two different events share an id")
	}
}

func TestTriggerMatches(t *testing.T) {
	t.Parallel()

	e := sample()
	tests := []struct {
		name    string
		trigger EventTrigger
		want    bool
	}{
		{"type and bucket", EventTrigger{EventType: e.Type, Resource: "uploads"}, true},
		{"type only", EventTrigger{EventType: e.Type}, true},
		{"another bucket", EventTrigger{EventType: e.Type, Resource: "other"}, false},
		{"another type", EventTrigger{EventType: "google.storage.object.delete", Resource: "uploads"}, false},
		// A bucket whose name ends in another's must not match it.
		{"a suffix of the bucket name", EventTrigger{EventType: e.Type, Resource: "loads"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.trigger.matches(e); got != tc.want {
				t.Errorf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}
