package functions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monirz/cloudrig/core/events"
)

// EventTrigger makes a function run when something happens rather than when it
// is called.
type EventTrigger struct {
	// EventType is the CloudEvents type to listen for, e.g.
	// google.cloud.storage.object.v1.finalized.
	EventType string

	// Resource narrows the trigger, e.g. a bucket name. Empty listens to every
	// source of that type.
	Resource string
}

// IsSet reports whether a trigger was configured.
func (t EventTrigger) IsSet() bool { return t.EventType != "" }

// matches reports whether an event should run this trigger.
func (t EventTrigger) matches(e events.Event) bool {
	if t.EventType != "" && t.EventType != e.Type {
		return false
	}
	// Resource is matched as a suffix of the source, so a bucket name works
	// without the caller spelling the whole //storage.googleapis.com form.
	return t.Resource == "" || strings.HasSuffix(e.Source, "/"+t.Resource)
}

// Retry bounds a failed delivery. GCF retries a background function, so a
// function that is briefly unwell should not silently lose an event.
const (
	// MaxDeliveryAttempts includes the first try.
	MaxDeliveryAttempts = 5

	// RetryBackoff is the first wait; it doubles each attempt.
	RetryBackoff = 200 * time.Millisecond
)

// subscribe wires an instance to the bus and returns the unsubscribe function.
func (r *Registry) subscribe(inst *Instance, t EventTrigger) func() {
	if r.bus == nil || !t.IsSet() {
		return nil
	}
	return r.bus.Subscribe(t.matches, func(ctx context.Context, e events.Event) {
		r.deliverWithRetry(ctx, inst, e)
	})
}

// deliverWithRetry backs off through the injected clock rather than sleeping,
// so a test drives the retries by advancing time and never waits on one.
func (r *Registry) deliverWithRetry(ctx context.Context, inst *Instance, e events.Event) {
	backoff := RetryBackoff

	for attempt := 1; ; attempt++ {
		err := deliver(ctx, inst, e)
		if err == nil {
			if attempt > 1 {
				r.logf("trigger: %s: delivered on attempt %d", inst.Name(), attempt)
			}
			return
		}
		if attempt >= MaxDeliveryAttempts {
			r.logf("trigger: %s: giving up after %d attempts: %v", inst.Name(), attempt, err)
			return
		}
		r.logf("trigger: %s: attempt %d failed, retrying: %v", inst.Name(), attempt, err)

		if !r.wait(ctx, backoff) {
			r.logf("trigger: %s: abandoned while waiting to retry", inst.Name())
			return
		}
		backoff *= 2
	}
}

// wait blocks for d on the injected clock, reporting whether it elapsed rather
// than the context ending. It exists because the emulator's own clock may be a
// FakeClock, where sleeping would wait forever.
func (r *Registry) wait(ctx context.Context, d time.Duration) bool {
	done := make(chan struct{})
	timer := r.clk.AfterFunc(d, func() { close(done) })

	select {
	case <-done:
		return true
	case <-ctx.Done():
		timer.Stop()
		return false
	}
}

// deliver posts the event to the function.
//
// A triggered function is reached over HTTP like any other: that is how the
// functions frameworks serve event handlers too, so one transport carries both
// kinds and the runner needs no second path.
func deliver(ctx context.Context, inst *Instance, e events.Event) error {
	body, err := json.Marshal(backgroundEvent(e))
	if err != nil {
		return fmt.Errorf("encoding the event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.ContentLength = int64(len(body))

	rec := &statusRecorder{header: http.Header{}, status: http.StatusOK}
	inst.ServeHTTP(rec, req)

	if rec.status >= http.StatusBadRequest {
		return fmt.Errorf("the function answered %d: %s", rec.status, rec.body.String())
	}
	return nil
}

// backgroundEvent is the first-generation envelope: a data payload beside a
// context describing what happened.
//
// Not CloudEvents. functions-framework reads body.data and body.context, and
// falls back to treating every non-data field as the context when there is
// none — so a CloudEvent body makes a gen1 handler read specversion and
// datacontenttype as its context and find no eventType at all.
func backgroundEvent(e events.Event) map[string]any {
	return map[string]any{
		"data": e.Data,
		"context": map[string]any{
			"eventId":   eventID(e),
			"timestamp": e.Time.UTC().Format("2006-01-02T15:04:05.999999999Z"),
			"eventType": e.Type,
			"resource": map[string]any{
				"name":    e.Resource,
				"service": e.Service,
				"type":    e.Kind,
			},
		},
	}
}

// eventID is stable for one occurrence, so a retry carries the id of the
// delivery it repeats rather than looking like a new event.
func eventID(e events.Event) string {
	sum := sha256.Sum256([]byte(e.Type + "\x00" + e.Resource + "\x00" +
		e.Time.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])[:32]
}

// statusRecorder captures a triggered function's response, which nothing
// returns to a caller: there is no caller, only a log line if it failed.
type statusRecorder struct {
	header  http.Header
	body    bytes.Buffer
	status  int
	written bool
}

func (r *statusRecorder) Header() http.Header { return r.header }

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.written = true
	// Bounded: a chatty failure should not be held in full to be logged.
	if r.body.Len() < 4<<10 {
		return r.body.Write(p)
	}
	return len(p), nil
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.written {
		r.status = status
		r.written = true
	}
}
