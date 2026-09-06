package cloudscheduler

import (
	"context"
	"google.golang.org/protobuf/types/known/durationpb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestValidJobName(t *testing.T) {
	t.Parallel()
	if err := validJobName("projects/p/locations/l/jobs/j"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	for _, bad := range []string{"", "projects/p/jobs/j", "projects/p/locations/l/jobs/"} {
		if status.Code(validJobName(bad)) != codes.InvalidArgument {
			t.Errorf("validJobName(%q) not rejected", bad)
		}
	}
}

// counter is a Pub/Sub publish sink for the unit tests: it records how many
// times a job delivered.
type counter struct{ n int }

func (c *counter) publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	c.n++
	return nil
}

// TestRecurringFire is the core behaviour without a network: an hourly job
// fires once per advanced hour.
func TestRecurringFire(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(epoch)
	c := &counter{}
	s := New(store.NewMemory(), clk, c.publish)
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: "projects/p/locations/l",
		Job: &schedulerpb.Job{
			Name: "projects/p/locations/l/jobs/h", Schedule: "0 * * * *",
			Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
				TopicName: "projects/p/topics/t", Data: []byte("x"),
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		clk.Advance(time.Hour)
	}
	if c.n != 5 {
		t.Errorf("fired %d times in 5 advanced hours, want 5", c.n)
	}
}

// TestRestartReArmsJobs holds that a service built over an existing store keeps
// firing its jobs — the fork/restart case.
func TestRestartReArmsJobs(t *testing.T) {
	t.Parallel()

	kv := store.NewMemory()
	clk := clock.NewFake(epoch)
	first := &counter{}
	s1 := New(kv, clk, first.publish)
	if _, err := s1.CreateJob(context.Background(), &schedulerpb.CreateJobRequest{
		Parent: "projects/p/locations/l",
		Job: &schedulerpb.Job{
			Name: "projects/p/locations/l/jobs/h", Schedule: "0 * * * *",
			Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
				TopicName: "projects/p/topics/t",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Fresh service over the same store.
	second := &counter{}
	New(kv, clk, second.publish)
	clk.Advance(2 * time.Hour)
	if second.n == 0 {
		t.Error("a job inherited from the store never fired after restart")
	}
}

// TestPausedJobArmsNothing is the pause half at the unit level.
func TestPausedJobArmsNothing(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(epoch)
	c := &counter{}
	s := New(store.NewMemory(), clk, c.publish)
	ctx := context.Background()
	name := "projects/p/locations/l/jobs/h"

	s.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: "projects/p/locations/l",
		Job: &schedulerpb.Job{
			Name: name, Schedule: "* * * * *",
			Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{TopicName: "t"}},
		},
	})
	if _, err := s.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Minute)
	if c.n != 0 {
		t.Errorf("a paused job fired %d times", c.n)
	}
	s.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: name})
	clk.Advance(2 * time.Minute)
	if c.n == 0 {
		t.Error("a resumed job never fired")
	}
}

// TestCreateJobRejectsForeignName holds that a job's name must live under the
// parent it was created in, or it would run in a scope nobody named.
func TestCreateJobRejectsForeignName(t *testing.T) {
	t.Parallel()

	s := New(store.NewMemory(), clock.NewFake(epoch), nil)
	ctx := context.Background()

	_, err := s.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: "projects/p/locations/A",
		Job: &schedulerpb.Job{
			Name:     "projects/p/locations/B/jobs/x",
			Schedule: "0 * * * *",
			Target:   &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{TopicName: "t"}},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument for a name outside the parent", err)
	}

	// The matching name is accepted.
	if _, err := s.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: "projects/p/locations/A",
		Job: &schedulerpb.Job{
			Name:     "projects/p/locations/A/jobs/ok",
			Schedule: "0 * * * *",
			Target:   &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{TopicName: "t"}},
		},
	}); err != nil {
		t.Errorf("a name under the parent was rejected: %v", err)
	}
}

// TestStalledHTTPTargetDoesNotPileUp holds that a target which accepts a
// connection and never answers cannot accumulate one stuck goroutine per
// recurrence, nor hang Sync forever: each attempt is deadline-bounded.
func TestStalledHTTPTargetDoesNotPileUp(t *testing.T) {
	t.Parallel()

	// A handler that blocks until the test ends, standing in for a hung server.
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // the client's deadline cancels the request
	}))
	defer srv.Close()

	clk := clock.NewFake(epoch)
	s := New(store.NewMemory(), clk, nil)
	ctx := context.Background()

	if _, err := s.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: "projects/p/locations/l",
		Job: &schedulerpb.Job{
			Name:            "projects/p/locations/l/jobs/hung",
			Schedule:        "* * * * *", // every minute
			AttemptDeadline: durationpb.New(50 * time.Millisecond),
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
				Uri: srv.URL, HttpMethod: schedulerpb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Fire several occurrences.
	for i := 0; i < 5; i++ {
		clk.Advance(time.Minute)
	}
	// Each attempt is bounded by the 50ms deadline, so Sync returns rather than
	// blocking on a stuck goroutine.
	done := make(chan struct{})
	go func() { s.Sync(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Sync blocked on stalled deliveries")
	}
}
