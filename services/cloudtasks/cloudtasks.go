// Package cloudtasks is the Cloud Tasks emulation.
//
// A task is deferred HTTP work: it names a URL, a schedule time and a body, and
// the queue dispatches it when that time arrives, retrying on failure. The
// dispatch runs on the injected clock, so a test advances time to fire a task
// due in an hour rather than waiting one — which is the thing that cannot be
// done against real Cloud Tasks.
package cloudtasks

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service holds queues and their tasks, and dispatches due tasks.
type Service struct {
	cloudtaskspb.UnimplementedCloudTasksServer

	kv   store.Store
	clk  clock.Clock
	http httpDoer

	mu     sync.Mutex
	queues map[string]*queue // keyed by queue resource name
	seq    uint64            // task-id source for auto-named tasks

	// inFlight tracks due-now dispatches, which run on their own goroutine.
	// Sync waits for them, so a test can advance the clock knowing a previous
	// attempt has finished scheduling its retry.
	inFlight sync.WaitGroup
}

// Sync waits for every due-now dispatch to finish, including arming any retry
// it scheduled. Timer-driven dispatches run synchronously inside the clock's
// Advance and need no waiting; only the immediate goroutine path does.
func (s *Service) Sync() { s.inFlight.Wait() }

// httpDoer sends a task's request. It is an interface so a test can observe
// dispatches without a real server.
type httpDoer interface {
	do(ctx context.Context, t *cloudtaskspb.Task) (status int, err error)
}

// New wires a service. A nil doer uses the real HTTP client.
func New(kv store.Store, clk clock.Clock) *Service {
	s := &Service{
		kv:     kv,
		clk:    clk,
		http:   realDoer{},
		queues: map[string]*queue{},
	}
	// A store may already hold queues and tasks — a restart, or a fork. Re-arm
	// them so they still dispatch rather than sitting inert.
	s.recover()
	return s
}

// errNoHTTPTarget is returned for a task with no HTTP request; App Engine
// targets are not emulated.
var errNoHTTPTarget = fmt.Errorf("only HTTP targets are supported")

// Key layout. Queues and tasks are addressed by resource name, which carries
// the project and location already.
func queueKey(name string) string { return "ct/q/" + name }
func taskKey(name string) string  { return "ct/t/" + name }

func taskPrefix(queue string) string { return "ct/t/" + queue + "/tasks/" }

// validQueueName checks projects/{p}/locations/{l}/queues/{q}.
func validQueueName(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" ||
		parts[4] != "queues" || parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return status.Errorf(codes.InvalidArgument,
			"invalid queue name %q; expected projects/{p}/locations/{l}/queues/{q}", name)
	}
	return nil
}

// parseTaskParent splits a queue name from a task-create parent.
func parseTaskParent(parent string) (string, error) {
	if err := validQueueName(parent); err != nil {
		return "", err
	}
	return parent, nil
}

func (s *Service) getQueueRecord(ctx context.Context, name string) (*cloudtaskspb.Queue, error) {
	raw, _, err := s.kv.Get(ctx, queueKey(name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Queue does not exist: %s", name)
	}
	var q cloudtaskspb.Queue
	if err := unmarshal.Unmarshal(raw, &q); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding queue: %v", err)
	}
	return &q, nil
}

func (s *Service) getTaskRecord(ctx context.Context, name string) (*cloudtaskspb.Task, error) {
	raw, _, err := s.kv.Get(ctx, taskKey(name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Task does not exist: %s", name)
	}
	var t cloudtaskspb.Task
	if err := unmarshal.Unmarshal(raw, &t); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding task: %v", err)
	}
	return &t, nil
}

func (s *Service) nextTaskID() string {
	s.seq++
	return fmt.Sprintf("%d", s.seq)
}
