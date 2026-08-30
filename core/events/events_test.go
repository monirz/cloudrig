package events_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/events"
)

func event(kind, subject string) events.Event {
	return events.Event{Type: kind, Subject: subject, Time: time.Unix(0, 0)}
}

func TestPublishReachesSubscribers(t *testing.T) {
	t.Parallel()

	bus := events.New()
	var mu sync.Mutex
	var got []string

	bus.Subscribe(nil, func(_ context.Context, e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e.Subject)
	})

	bus.Publish(context.Background(), event("x", "a"))
	bus.Publish(context.Background(), event("x", "b"))
	bus.Sync()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Errorf("delivered %v, want two events", got)
	}
}

func TestMatchFiltersEvents(t *testing.T) {
	t.Parallel()

	bus := events.New()
	var mu sync.Mutex
	seen := 0

	bus.Subscribe(func(e events.Event) bool { return strings.HasSuffix(e.Type, "finalized") },
		func(context.Context, events.Event) {
			mu.Lock()
			seen++
			mu.Unlock()
		})

	bus.Publish(context.Background(), event("object.finalized", "a"))
	bus.Publish(context.Background(), event("object.deleted", "b"))
	bus.Sync()

	mu.Lock()
	defer mu.Unlock()
	if seen != 1 {
		t.Errorf("handler ran %d times, want 1", seen)
	}
}

// TestSyncWaitsForDelivery is what makes an event-driven test deterministic:
// without it an assertion would have to poll or sleep.
func TestSyncWaitsForDelivery(t *testing.T) {
	t.Parallel()

	bus := events.New()
	done := false
	bus.Subscribe(nil, func(context.Context, events.Event) {
		time.Sleep(20 * time.Millisecond)
		done = true
	})

	bus.Publish(context.Background(), event("x", "a"))
	bus.Sync()

	// Read without synchronisation on purpose: if Sync returned early this is
	// a data race and -race says so.
	if !done {
		t.Error("Sync returned before the handler ran")
	}
}

func TestUnsubscribe(t *testing.T) {
	t.Parallel()

	bus := events.New()
	var mu sync.Mutex
	seen := 0

	stop := bus.Subscribe(nil, func(context.Context, events.Event) {
		mu.Lock()
		seen++
		mu.Unlock()
	})

	bus.Publish(context.Background(), event("x", "a"))
	bus.Sync()
	stop()
	bus.Publish(context.Background(), event("x", "b"))
	bus.Sync()

	mu.Lock()
	defer mu.Unlock()
	if seen != 1 {
		t.Errorf("handler ran %d times after unsubscribing, want 1", seen)
	}
}

// TestPublishDoesNotBlockThePublisher holds the rule that an object write must
// not wait on whatever a function does with the news.
func TestPublishDoesNotBlockThePublisher(t *testing.T) {
	t.Parallel()

	bus := events.New()
	release := make(chan struct{})
	bus.Subscribe(nil, func(context.Context, events.Event) { <-release })

	returned := make(chan struct{})
	go func() {
		bus.Publish(context.Background(), event("x", "a"))
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	close(release)
	bus.Sync()
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	t.Parallel()

	bus := events.New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			stop := bus.Subscribe(nil, func(context.Context, events.Event) {})
			stop()
		}()
		go func() {
			defer wg.Done()
			bus.Publish(context.Background(), event("x", "a"))
		}()
	}
	wg.Wait()
	bus.Sync()
}
