package cloudtasks

import (
	"context"
	"sort"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CreateTask stores a task and arms its dispatch. A task with no schedule time
// is due now.
func (s *Service) CreateTask(ctx context.Context, req *cloudtaskspb.CreateTaskRequest) (*cloudtaskspb.Task, error) {
	queueName, err := parseTaskParent(req.GetParent())
	if err != nil {
		return nil, err
	}

	task := req.GetTask()
	if task == nil {
		return nil, status.Error(codes.InvalidArgument, "task is required")
	}
	if task.GetAppEngineHttpRequest() != nil {
		return nil, status.Error(codes.Unimplemented,
			"App Engine task targets are not supported; use an http_request")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getQueueRecord(ctx, queueName); err != nil {
		return nil, err
	}

	now := s.clk.Now()
	if task.GetName() == "" {
		task.Name = queueName + "/tasks/" + s.nextTaskID()
	} else if queueOf(task.GetName()) != queueName {
		// An explicit task name must live under the parent queue. Otherwise it
		// would be stored and dispatched under a queue the caller did not name
		// — invisible in the parent's listing and following the wrong queue's
		// pause and retry state.
		return nil, status.Errorf(codes.InvalidArgument,
			"task name %q is not under the parent queue %q", task.GetName(), queueName)
	}
	task.CreateTime = timestamppb.New(now)
	if task.GetScheduleTime() == nil {
		task.ScheduleTime = timestamppb.New(now)
	}

	if err := s.putTask(ctx, task); err != nil {
		return nil, status.Errorf(codes.AlreadyExists, "Task already exists: %s", task.GetName())
	}
	s.schedule(task.GetName(), task.GetScheduleTime().AsTime())
	return task, nil
}

func (s *Service) GetTask(ctx context.Context, req *cloudtaskspb.GetTaskRequest) (*cloudtaskspb.Task, error) {
	return s.getTaskRecord(ctx, req.GetName())
}

func (s *Service) ListTasks(ctx context.Context, req *cloudtaskspb.ListTasksRequest) (*cloudtaskspb.ListTasksResponse, error) {
	entries, _, err := s.kv.List(ctx, taskPrefix(req.GetParent()), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing tasks: %v", err)
	}
	out := make([]*cloudtaskspb.Task, 0, len(entries))
	for _, kv := range entries {
		var t cloudtaskspb.Task
		if err := unmarshal.Unmarshal(kv.Val, &t); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding task: %v", err)
		}
		out = append(out, &t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &cloudtaskspb.ListTasksResponse{Tasks: out}, nil
}

func (s *Service) DeleteTask(ctx context.Context, req *cloudtaskspb.DeleteTaskRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.kv.Delete(ctx, taskKey(req.GetName()), 0); err != nil {
		return nil, status.Errorf(codes.NotFound, "Task does not exist: %s", req.GetName())
	}
	if live := s.queues[queueOf(req.GetName())]; live != nil {
		if timer := live.timers[req.GetName()]; timer != nil {
			timer.Stop()
			delete(live.timers, req.GetName())
		}
	}
	return &emptypb.Empty{}, nil
}

// RunTask dispatches a task now, ahead of its schedule. It is how an operator
// forces a task without waiting for its time.
func (s *Service) RunTask(ctx context.Context, req *cloudtaskspb.RunTaskRequest) (*cloudtaskspb.Task, error) {
	s.mu.Lock()
	task, err := s.getTaskRecord(ctx, req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if live := s.queues[queueOf(req.GetName())]; live != nil {
		if timer := live.timers[req.GetName()]; timer != nil {
			timer.Stop()
			delete(live.timers, req.GetName())
		}
	}
	s.mu.Unlock()

	s.dispatch(req.GetName())

	// Report the task as it stood when RunTask was asked; it may already be
	// gone if the dispatch succeeded, which is fine.
	return task, nil
}
