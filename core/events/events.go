// Package events is the in-process bus services use to reach each other.
//
// It exists so they need not: a bucket knows nothing about functions, and a
// function knows nothing about buckets. Both know this bus, which is the only
// way anything here crosses a service boundary.
package events

import (
	"context"
	"sync"
	"time"
)

// Event is something that happened.
type Event struct {
	// Type is the event name a trigger matches on, e.g.
	// google.storage.object.finalize.
	Type string

	// Source identifies what produced it, e.g.
	// //storage.googleapis.com/projects/_/buckets/my-bucket. A trigger matches
	// its resource against the tail of this.
	Source string

	// Subject names the thing within the source, e.g. objects/logs/app.log.
	Subject string

	// Resource, Service and Kind describe the affected resource the way a
	// first-generation function's context reports it.
	Resource string
	Service  string
	Kind     string

	Time time.Time

	// Data is the payload, in the shape the service's API returns it.
	Data map[string]any
}

// Handler receives an event. It runs on the bus's own goroutine, not the
// publisher's, so a slow subscriber cannot stall an upload.
type Handler func(context.Context, Event)

// Match decides whether a subscriber wants an event.
type Match func(Event) bool

// Bus delivers events to subscribers.
type Bus struct {
	mu     sync.RWMutex
	next   uint64
	subs   map[uint64]subscription
	inWork sync.WaitGroup
}

type subscription struct {
	match Match
	fn    Handler
}

// New returns an empty bus.
func New() *Bus { return &Bus{subs: map[uint64]subscription{}} }

// Subscribe registers a handler for events match accepts, and returns a
// function that removes it.
func (b *Bus) Subscribe(match Match, fn Handler) func() {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.next
	b.next++
	b.subs[id] = subscription{match: match, fn: fn}

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, id)
	}
}

// Publish delivers an event to every interested subscriber, asynchronously.
//
// The publisher is never blocked: an object write must not wait on whatever a
// function does with the news. Use Sync to wait for delivery.
func (b *Bus) Publish(ctx context.Context, e Event) {
	b.mu.RLock()
	interested := make([]Handler, 0, len(b.subs))
	for _, s := range b.subs {
		if s.match == nil || s.match(e) {
			interested = append(interested, s.fn)
		}
	}
	b.mu.RUnlock()

	// Detached from the publisher's context: an event outlives the request
	// that produced it, and delivery must not be cancelled the instant that
	// request is answered. Values are kept, cancellation is not.
	delivery := context.WithoutCancel(ctx)

	for _, fn := range interested {
		b.inWork.Add(1)
		go func(fn Handler) {
			defer b.inWork.Done()
			fn(delivery, e)
		}(fn)
	}
}

// Sync waits for every event published so far to be delivered.
//
// Delivery is asynchronous, so without this a test would have to poll or sleep
// to see an effect. It is what makes an event-driven assertion deterministic.
func (b *Bus) Sync() { b.inWork.Wait() }
