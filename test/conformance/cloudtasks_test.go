package conformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func ctClient(t *testing.T) (*cloudtasks.Client, *cloudrig.Emulator, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := cloudtasks.NewClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("cloudtasks.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, emu, ctx
}

const ctParent = "projects/test-project/locations/us-central1"

func mkQueue(t *testing.T, c *cloudtasks.Client, ctx context.Context, id string) string {
	t.Helper()
	q, err := c.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: ctParent,
		Queue:  &cloudtaskspb.Queue{Name: ctParent + "/queues/" + id},
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return q.GetName()
}

// sink is an HTTP target that records the bodies it received.
type sink struct {
	mu   sync.Mutex
	got  []string
	code int
}

func (s *sink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		s.mu.Lock()
		s.got = append(s.got, string(body))
		code := s.code
		s.mu.Unlock()
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
	}
}

func (s *sink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.got) }

// TestTaskFiresAtItsScheduledTime is the whole point: a task due in an hour
// fires when the test advances the clock an hour, not after a real hour.
func TestTaskFiresAtItsScheduledTime(t *testing.T) {
	c, emu, ctx := ctClient(t)
	queue := mkQueue(t, c, ctx, "scheduled")

	target := &sink{}
	srv := httptest.NewServer(target.handler())
	t.Cleanup(srv.Close)

	// Due one hour from the emulator's clock.
	when := emu.Clock().Now().Add(time.Hour)
	if _, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue,
		Task: &cloudtaskspb.Task{
			ScheduleTime: timestamp(when),
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url:        srv.URL,
				HttpMethod: cloudtaskspb.HttpMethod_POST,
				Body:       []byte("scheduled-body"),
			}},
		},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Nothing fires before its time.
	emu.FakeClock(t).Advance(59 * time.Minute)
	emu.SyncEvents()
	if got := target.count(); got != 0 {
		t.Errorf("the task fired early: %d dispatches", got)
	}

	// Cross the schedule time and it fires.
	emu.FakeClock(t).Advance(2 * time.Minute)
	waitForCount(t, target, 1)
	if got := target.got[0]; got != "scheduled-body" {
		t.Errorf("body = %q", got)
	}
}

// TestTaskRetriesOnFailure covers the retry policy, driven by the clock: a
// failing target is retried with backoff until the attempt cap.
func TestTaskRetriesOnFailure(t *testing.T) {
	c, emu, ctx := ctClient(t)

	target := &sink{code: http.StatusInternalServerError}
	srv := httptest.NewServer(target.handler())
	t.Cleanup(srv.Close)

	q, err := c.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: ctParent,
		Queue: &cloudtaskspb.Queue{
			Name: ctParent + "/queues/retrying",
			RetryConfig: &cloudtaskspb.RetryConfig{
				MaxAttempts: 3,
				MinBackoff:  duration(time.Second),
				MaxBackoff:  duration(time.Minute),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: q.GetName(),
		Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: srv.URL, HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// First attempt is immediate.
	waitForCount(t, target, 1)
	// Backoff doubles: 1s, then 2s. Advancing past both drives the retries.
	emu.FakeClock(t).Advance(time.Second)
	waitForCount(t, target, 2)
	emu.FakeClock(t).Advance(2 * time.Second)
	waitForCount(t, target, 3)

	// Three attempts is the cap; no fourth however far time advances.
	emu.FakeClock(t).Advance(time.Hour)
	emu.SyncEvents()
	if got := target.count(); got != 3 {
		t.Errorf("attempts = %d, want 3 (the cap)", got)
	}
}

// TestRunTaskFiresImmediately covers forcing a task ahead of its schedule.
func TestRunTaskFiresImmediately(t *testing.T) {
	c, emu, ctx := ctClient(t)
	queue := mkQueue(t, c, ctx, "forced")

	target := &sink{}
	srv := httptest.NewServer(target.handler())
	t.Cleanup(srv.Close)

	task, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue,
		Task: &cloudtaskspb.Task{
			ScheduleTime: timestamp(emu.Clock().Now().Add(24 * time.Hour)),
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: srv.URL, HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without advancing the clock at all, RunTask dispatches it now.
	if _, err := c.RunTask(ctx, &cloudtaskspb.RunTaskRequest{Name: task.GetName()}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	waitForCount(t, target, 1)
}

// TestPausedQueueHoldsTasks holds that a paused queue fires nothing until it
// resumes.
func TestPausedQueueHoldsTasks(t *testing.T) {
	c, emu, ctx := ctClient(t)
	queue := mkQueue(t, c, ctx, "paused")

	target := &sink{}
	srv := httptest.NewServer(target.handler())
	t.Cleanup(srv.Close)

	if _, err := c.PauseQueue(ctx, &cloudtaskspb.PauseQueueRequest{Name: queue}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue,
		Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: srv.URL, HttpMethod: cloudtaskspb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	emu.FakeClock(t).Advance(time.Hour)
	emu.SyncEvents()
	if got := target.count(); got != 0 {
		t.Errorf("a paused queue fired %d tasks", got)
	}

	if _, err := c.ResumeQueue(ctx, &cloudtaskspb.ResumeQueueRequest{Name: queue}); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, target, 1)
}

func TestCloudTasksErrors(t *testing.T) {
	c, _, ctx := ctClient(t)

	mkQueue(t, c, ctx, "dup")
	if _, err := c.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: ctParent,
		Queue:  &cloudtaskspb.Queue{Name: ctParent + "/queues/dup"},
	}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate queue = %v, want AlreadyExists", err)
	}
	if _, err := c.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{
		Name: ctParent + "/queues/ghost",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("missing queue = %v, want NotFound", err)
	}
	if _, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: ctParent + "/queues/ghost",
		Task:   &cloudtaskspb.Task{},
	}); status.Code(err) != codes.NotFound {
		t.Errorf("task on a missing queue = %v, want NotFound", err)
	}
}

func timestamp(t time.Time) *timestamppb.Timestamp  { return timestamppb.New(t) }
func duration(d time.Duration) *durationpb.Duration { return durationpb.New(d) }

// waitForCount waits until the sink has received n dispatches. Dispatch runs on
// the bus goroutine, so a fired timer's HTTP call lands shortly after Advance
// returns.
func waitForCount(t *testing.T, s *sink, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d dispatches arrived", s.count(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
