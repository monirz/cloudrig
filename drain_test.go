package cloudrig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/events"
	"github.com/monirz/cloudrig/services/cloudscheduler"
	"github.com/monirz/cloudrig/services/cloudtasks"
	"github.com/monirz/cloudrig/store"
)

// TestDrainAsyncWaitsForTransitiveWork holds that drainAsync loops until work is
// quiescent, not just one pass. A due task is the last stage drained, but its
// delivery publishes a bus event — work a single scheduler→bus→tasks pass would
// miss, because the bus was already synced. drainAsync must catch it on a
// further pass, so the event is delivered before it returns.
func TestDrainAsyncWaitsForTransitiveWork(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	bus := events.New()
	ct := cloudtasks.New(store.NewMemory(), clk)
	cs := cloudscheduler.New(store.NewMemory(), clk,
		func(context.Context, string, []byte, map[string]string) error { return nil })

	var delivered atomic.Int32
	bus.Subscribe(func(events.Event) bool { return true },
		func(context.Context, events.Event) { delivered.Add(1) })

	// The task's target publishes to the bus when hit — the transitive work.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bus.Publish(context.Background(), events.Event{Type: "task.done"})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	const queue = "projects/p/locations/l/queues/q"
	if _, err := ct.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: "projects/p/locations/l",
		Queue:  &cloudtaskspb.Queue{Name: queue},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ct.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue, // no schedule time: due now, dispatched on a goroutine
		Task: &cloudtaskspb.Task{MessageType: &cloudtaskspb.Task_HttpRequest{
			HttpRequest: &cloudtaskspb.HttpRequest{Url: srv.URL, HttpMethod: cloudtaskspb.HttpMethod_POST},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	drainAsync(cs, bus, ct)

	if got := delivered.Load(); got != 1 {
		t.Errorf("drain returned before the task-triggered event was delivered: delivered=%d, want 1", got)
	}
}
