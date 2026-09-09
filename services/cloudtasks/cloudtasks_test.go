package cloudtasks

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestValidQueueName(t *testing.T) {
	t.Parallel()

	if err := validQueueName("projects/p/locations/l/queues/q"); err != nil {
		t.Errorf("a valid name was rejected: %v", err)
	}
	for _, bad := range []string{
		"", "projects/p/locations/l/queues", "projects/p/queues/q",
		"projects//locations/l/queues/q", "projects/p/locations/l/queues/",
	} {
		if err := validQueueName(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("validQueueName(%q) = %v, want InvalidArgument", bad, err)
		}
	}
}

// TestQueueOf pins the split every dispatch relies on: a task name's queue is
// the part before /tasks/.
func TestQueueOf(t *testing.T) {
	t.Parallel()

	const task = "projects/p/locations/l/queues/q/tasks/42"
	if got := queueOf(task); got != "projects/p/locations/l/queues/q" {
		t.Errorf("queueOf = %q", got)
	}
	// A name without the marker is its own queue.
	if got := queueOf("projects/p/locations/l/queues/q"); got != "projects/p/locations/l/queues/q" {
		t.Errorf("queueOf(bare) = %q", got)
	}
}

// recorder is a test httpDoer: it counts dispatches and returns a fixed status.
type recorder struct {
	mu    sync.Mutex
	calls int
	code  int
	err   error
}

func (r *recorder) do(ctx context.Context, t *cloudtaskspb.Task) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	code := r.code
	if code == 0 {
		code = http.StatusOK
	}
	return code, r.err
}

func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.calls }

// TestBackoffDoublesAndCaps drives the retry policy directly, without a server,
// so the timing is exact: attempts happen only as the clock advances.
func TestBackoffDoublesAndCaps(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(epoch)
	s := New(store.NewMemory(), clk)
	rec := &recorder{code: http.StatusInternalServerError}
	s.http = rec

	const queue = "projects/p/locations/l/queues/q"
	ctx := context.Background()
	if _, err := s.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: "projects/p/locations/l",
		Queue: &cloudtaskspb.Queue{
			Name: queue,
			RetryConfig: &cloudtaskspb.RetryConfig{
				MaxAttempts: 3,
				MinBackoff:  durationProto(time.Second),
				MaxBackoff:  durationProto(time.Minute),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue,
		Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: "http://sink", HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The first attempt is immediate and runs on a goroutine; Sync drains it
	// AND the retry timer it arms, so the clock cannot be advanced through the
	// gap between the dispatch and its rescheduling. waitCalls would race there:
	// the count is bumped during the request, the retry armed only after it.
	s.Sync()
	if got := rec.count(); got != 1 {
		t.Fatalf("first attempt: %d dispatches, want 1", got)
	}
	// Backoff 1s → second attempt. A timer-driven dispatch runs synchronously
	// inside Advance and arms the next retry before Advance returns, so the
	// count is exact with no waiting.
	clk.Advance(time.Second)
	if got := rec.count(); got != 2 {
		t.Fatalf("after 1s: %d dispatches, want 2", got)
	}
	// Backoff doubles to 2s → third attempt.
	clk.Advance(2 * time.Second)
	if got := rec.count(); got != 3 {
		t.Fatalf("after 2s more: %d dispatches, want 3", got)
	}
	// The cap holds: no fourth, ever.
	clk.Advance(time.Hour)
	if got := rec.count(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}

	// The task is gone once it exhausts its attempts.
	if _, err := s.GetTask(ctx, &cloudtaskspb.GetTaskRequest{
		Name: queue + "/tasks/1",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("an exhausted task is still stored: %v", err)
	}
}

// TestSuccessDeletesTheTask holds that a 2xx dispatch retires the task.
func TestSuccessDeletesTheTask(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(epoch)
	s := New(store.NewMemory(), clk)
	rec := &recorder{code: http.StatusOK}
	s.http = rec

	const queue = "projects/p/locations/l/queues/q"
	ctx := context.Background()
	s.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: "projects/p/locations/l",
		Queue:  &cloudtaskspb.Queue{Name: queue},
	})
	task, err := s.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue,
		Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: "http://sink", HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitCalls(t, rec, 1)
	deadline := time.Now().Add(time.Second)
	for {
		_, err := s.GetTask(ctx, &cloudtaskspb.GetTaskRequest{Name: task.GetName()})
		if status.Code(err) == codes.NotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a succeeded task was not deleted")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func durationProto(d time.Duration) *durationpb.Duration { return durationpb.New(d) }

func waitCalls(t *testing.T, r *recorder, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d dispatches", r.count(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRestartReArmsStoredTasks is the fork/restart case: a service built over a
// store that already holds a queue and a task must still dispatch it, rather
// than starting with an empty runtime map and leaving the task inert.
func TestRestartReArmsStoredTasks(t *testing.T) {
	t.Parallel()

	kv := store.NewMemory()
	clk := clock.NewFake(epoch)
	ctx := context.Background()
	const queue = "projects/p/locations/l/queues/q"

	first := New(kv, clk)
	if _, err := first.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: "projects/p/locations/l", Queue: &cloudtaskspb.Queue{Name: queue},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue,
		Task: &cloudtaskspb.Task{
			ScheduleTime: durationFromNow(clk, time.Hour),
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: "http://sink", HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// A fresh service over the same store — a restart or a fork.
	second := New(kv, clk)
	rec := &recorder{code: http.StatusOK}
	second.http = rec

	clk.Advance(2 * time.Hour) // the task was due at +1h
	second.Sync()
	waitCalls(t, rec, 1)
}

// TestCreateTaskRejectsForeignName holds that an explicit task name must live
// under the parent queue, or it would dispatch under a queue nobody named.
func TestCreateTaskRejectsForeignName(t *testing.T) {
	t.Parallel()

	s := New(store.NewMemory(), clock.NewFake(epoch))
	ctx := context.Background()
	s.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: "projects/p/locations/l",
		Queue:  &cloudtaskspb.Queue{Name: "projects/p/locations/l/queues/A"},
	})

	_, err := s.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: "projects/p/locations/l/queues/A",
		Task: &cloudtaskspb.Task{
			Name: "projects/p/locations/l/queues/B/tasks/x",
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: "http://sink", HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument for a name under a different queue", err)
	}

	// The matching name is accepted.
	if _, err := s.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: "projects/p/locations/l/queues/A",
		Task: &cloudtaskspb.Task{
			Name: "projects/p/locations/l/queues/A/tasks/ok",
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: "http://sink", HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Errorf("a name under the parent queue was rejected: %v", err)
	}
}

func durationFromNow(clk clock.Clock, d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(clk.Now().Add(d))
}
