package main

import (
	"testing"

	"github.com/monirz/cloudrig/functions"
	"github.com/monirz/cloudrig/services/pubsub"
	"github.com/monirz/cloudrig/services/storage"
)

// TestTriggerFromFlags pins what each deploy flag means, including the
// defaults: naming a resource is enough, and the two kinds must not be
// confused with each other.
func TestTriggerFromFlags(t *testing.T) {
	cases := []struct {
		name  string
		flags deployFlags
		want  functions.EventTrigger
	}{
		{"none", deployFlags{}, functions.EventTrigger{}},
		{
			"a bucket alone defaults to finalize",
			deployFlags{triggerBucket: "uploads"},
			functions.EventTrigger{EventType: storage.EventFinalized, Resource: "uploads"},
		},
		{
			"a topic alone defaults to publish",
			deployFlags{triggerTopic: "orders"},
			functions.EventTrigger{EventType: pubsub.EventPublish, Resource: "orders"},
		},
		{
			"a bucket with an explicit event",
			deployFlags{triggerBucket: "uploads", triggerEvent: storage.EventDeleted},
			functions.EventTrigger{EventType: storage.EventDeleted, Resource: "uploads"},
		},
		{
			"an event alone listens to every bucket",
			deployFlags{triggerEvent: storage.EventArchived},
			functions.EventTrigger{EventType: storage.EventArchived},
		},
		{
			"a topic wins over a bucket, rather than merging the two",
			deployFlags{triggerTopic: "orders", triggerBucket: "uploads"},
			functions.EventTrigger{EventType: pubsub.EventPublish, Resource: "orders"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trigger(c.flags); got != c.want {
				t.Errorf("trigger() = %+v, want %+v", got, c.want)
			}
		})
	}
}
