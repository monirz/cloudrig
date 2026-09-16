package cloudtasks

import (
	"context"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// TestListDeletePurge covers the admin RPCs the dispatch tests do not: listing
// queues and tasks, deleting a task, and purging a queue.
func TestListDeletePurge(t *testing.T) {
	t.Parallel()

	s := New(store.NewMemory(), clock.NewFake(epoch))
	ctx := context.Background()
	const parent = "projects/p/locations/l"
	const queue = parent + "/queues/q"

	if _, err := s.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{
		Parent: parent,
		Queue:  &cloudtaskspb.Queue{Name: queue},
	}); err != nil {
		t.Fatal(err)
	}

	if ql, err := s.ListQueues(ctx, &cloudtaskspb.ListQueuesRequest{Parent: parent}); err != nil {
		t.Fatalf("ListQueues: %v", err)
	} else if len(ql.GetQueues()) != 1 {
		t.Fatalf("ListQueues = %d, want 1", len(ql.GetQueues()))
	}

	// An unreachable sink, so the task stays queued for a retry rather than
	// being dispatched and gone.
	task, err := s.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: queue,
		Task: &cloudtaskspb.Task{MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
			Url: "http://127.0.0.1:1/", HttpMethod: cloudtaskspb.HttpMethod_POST,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Sync()

	if tl, err := s.ListTasks(ctx, &cloudtaskspb.ListTasksRequest{Parent: queue}); err != nil {
		t.Fatalf("ListTasks: %v", err)
	} else if len(tl.GetTasks()) != 1 {
		t.Fatalf("ListTasks = %d, want 1", len(tl.GetTasks()))
	}

	if _, err := s.DeleteTask(ctx, &cloudtaskspb.DeleteTaskRequest{Name: task.GetName()}); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := s.PurgeQueue(ctx, &cloudtaskspb.PurgeQueueRequest{Name: queue}); err != nil {
		t.Fatalf("PurgeQueue: %v", err)
	}
	if tl, err := s.ListTasks(ctx, &cloudtaskspb.ListTasksRequest{Parent: queue}); err != nil {
		t.Fatalf("ListTasks after purge: %v", err)
	} else if len(tl.GetTasks()) != 0 {
		t.Errorf("ListTasks = %d after delete+purge, want 0", len(tl.GetTasks()))
	}
}
