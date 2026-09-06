package cloudtasks

import (
	"context"
	"sort"
	"strings"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Service) CreateQueue(ctx context.Context, req *cloudtaskspb.CreateQueueRequest) (*cloudtaskspb.Queue, error) {
	q := req.GetQueue()
	if q == nil || q.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "queue.name is required")
	}
	if err := validQueueName(q.GetName()); err != nil {
		return nil, err
	}
	q.State = cloudtaskspb.Queue_RUNNING

	encoded, err := marshal.Marshal(q)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding queue: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.kv.Put(ctx, queueKey(q.GetName()), encoded, 0); err != nil {
		return nil, status.Errorf(codes.AlreadyExists, "Queue already exists: %s", q.GetName())
	}
	s.queues[q.GetName()] = newQueue()
	return q, nil
}

func (s *Service) GetQueue(ctx context.Context, req *cloudtaskspb.GetQueueRequest) (*cloudtaskspb.Queue, error) {
	return s.getQueueRecord(ctx, req.GetName())
}

func (s *Service) ListQueues(ctx context.Context, req *cloudtaskspb.ListQueuesRequest) (*cloudtaskspb.ListQueuesResponse, error) {
	entries, _, err := s.kv.List(ctx, "ct/q/"+req.GetParent()+"/queues/", 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing queues: %v", err)
	}
	out := make([]*cloudtaskspb.Queue, 0, len(entries))
	for _, kv := range entries {
		var q cloudtaskspb.Queue
		if err := unmarshal.Unmarshal(kv.Val, &q); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding queue: %v", err)
		}
		out = append(out, &q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &cloudtaskspb.ListQueuesResponse{Queues: out}, nil
}

func (s *Service) DeleteQueue(ctx context.Context, req *cloudtaskspb.DeleteQueueRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.kv.Delete(ctx, queueKey(req.GetName()), 0); err != nil {
		return nil, status.Errorf(codes.NotFound, "Queue does not exist: %s", req.GetName())
	}
	s.stopQueue(ctx, req.GetName())
	return &emptypb.Empty{}, nil
}

func (s *Service) PurgeQueue(ctx context.Context, req *cloudtaskspb.PurgeQueueRequest) (*cloudtaskspb.Queue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, err := s.getQueueRecord(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	s.stopQueue(ctx, req.GetName())
	s.queues[req.GetName()] = newQueue() // keep it live, just emptied
	q.PurgeTime = timestamppb.New(s.clk.Now())
	return q, nil
}

func (s *Service) PauseQueue(ctx context.Context, req *cloudtaskspb.PauseQueueRequest) (*cloudtaskspb.Queue, error) {
	return s.setQueueState(ctx, req.GetName(), cloudtaskspb.Queue_PAUSED)
}

func (s *Service) ResumeQueue(ctx context.Context, req *cloudtaskspb.ResumeQueueRequest) (*cloudtaskspb.Queue, error) {
	return s.setQueueState(ctx, req.GetName(), cloudtaskspb.Queue_RUNNING)
}

// setQueueState pauses or resumes a queue. Pausing cancels pending dispatches;
// resuming re-arms every task still waiting, so a task scheduled while paused
// fires once the queue runs again.
func (s *Service) setQueueState(ctx context.Context, name string, state cloudtaskspb.Queue_State) (*cloudtaskspb.Queue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, err := s.getQueueRecord(ctx, name)
	if err != nil {
		return nil, err
	}
	q.State = state
	if _, err := s.kv.Put(ctx, queueKey(name), mustMarshal(q), versionOrZero(ctx, s, queueKey(name))); err != nil {
		return nil, status.Errorf(codes.Aborted, "updating queue: %v", err)
	}

	live := s.queues[name]
	if live == nil {
		live = newQueue()
		s.queues[name] = live
	}
	if state == cloudtaskspb.Queue_PAUSED {
		live.paused = true
		for taskName, timer := range live.timers {
			timer.Stop()
			delete(live.timers, taskName)
		}
	} else {
		live.paused = false
		s.rearm(ctx, name)
	}
	return q, nil
}

// rearm re-schedules every task still stored for a queue. The caller holds
// s.mu.
func (s *Service) rearm(ctx context.Context, queueName string) {
	entries, _, err := s.kv.List(ctx, taskPrefix(queueName), 0, "")
	if err != nil {
		return
	}
	for _, kv := range entries {
		var t cloudtaskspb.Task
		if err := unmarshal.Unmarshal(kv.Val, &t); err != nil {
			continue
		}
		s.schedule(t.GetName(), t.GetScheduleTime().AsTime())
	}
}

// stopQueue cancels a queue's timers and drops its tasks. The caller holds
// s.mu.
func (s *Service) stopQueue(ctx context.Context, name string) {
	if live := s.queues[name]; live != nil {
		for _, timer := range live.timers {
			timer.Stop()
		}
		delete(s.queues, name)
	}
	entries, _, err := s.kv.List(ctx, taskPrefix(name), 0, "")
	if err == nil {
		for _, kv := range entries {
			_ = s.kv.Delete(ctx, kv.Key, 0)
		}
	}
}

func mustMarshal(q *cloudtaskspb.Queue) []byte {
	b, _ := marshal.Marshal(q)
	return b
}

func versionOrZero(ctx context.Context, s *Service, key string) uint64 {
	_, v, err := s.kv.Get(ctx, key)
	if err != nil {
		return 0
	}
	return v
}

// trimQueueSuffix is a small helper for listing task parents.
func trimQueueSuffix(name string) string {
	return strings.TrimSuffix(name, "/")
}

var _ = store.ErrNotFound
