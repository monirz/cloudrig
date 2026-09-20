package cloudscheduler

import (
	"context"
	"testing"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// TestListAndUpdateJob covers the admin RPCs Terraform drives but the recurring
// tests do not: listing jobs and patching one in place.
func TestListAndUpdateJob(t *testing.T) {
	t.Parallel()

	s := New(store.NewMemory(), clock.NewFake(epoch), (&counter{}).publish)
	ctx := context.Background()
	const parent = "projects/p/locations/l"
	name := parent + "/jobs/h"

	create := func() error {
		_, err := s.CreateJob(ctx, &schedulerpb.CreateJobRequest{
			Parent: parent,
			Job: &schedulerpb.Job{
				Name: name, Schedule: "0 * * * *",
				Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
					TopicName: "projects/p/topics/t", Data: []byte("x"),
				}},
			},
		})
		return err
	}
	if err := create(); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListJobs(ctx, &schedulerpb.ListJobsRequest{Parent: parent})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(list.GetJobs()) != 1 {
		t.Fatalf("ListJobs = %d jobs, want 1", len(list.GetJobs()))
	}

	upd, err := s.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{
		Job: &schedulerpb.Job{Name: name, Schedule: "*/5 * * * *"},
	})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if upd.GetSchedule() != "*/5 * * * *" {
		t.Errorf("schedule = %q after update, want the new cron", upd.GetSchedule())
	}
}
