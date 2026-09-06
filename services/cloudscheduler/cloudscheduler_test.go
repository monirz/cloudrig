package cloudscheduler

import (
	"context"
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
