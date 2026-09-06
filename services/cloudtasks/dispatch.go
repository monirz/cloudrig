package cloudtasks

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig/core/clock"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Protos are stored as protojson, not encoding/json: a Task's target is a
// oneof, which a plain JSON decoder cannot rebuild.
var (
	marshal   = protojson.MarshalOptions{}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// Retry defaults, used when a queue sets none. These mirror Cloud Tasks'
// documented defaults closely enough for a local run.
const (
	defaultMaxAttempts = 3
	defaultMinBackoff  = 100 * time.Millisecond
	defaultMaxBackoff  = 10 * time.Second
)

// queue is a live queue: its record plus the timers for tasks awaiting
// dispatch. Timers run on the injected clock.
type queue struct {
	paused bool
	timers map[string]clock.Timer // task name -> its scheduled dispatch
}

func newQueue() *queue { return &queue{timers: map[string]clock.Timer{}} }

// schedule arms a task to dispatch at its schedule time. The caller holds s.mu.
func (s *Service) schedule(name string, when time.Time) {
	q := s.queues[queueOf(name)]
	if q == nil || q.paused {
		return
	}
	delay := when.Sub(s.clk.Now())
	if delay <= 0 {
		// Already due. A zero-delay timer on a FakeClock would not fire until
		// the next Advance, but a due task should dispatch now — so it runs on
		// its own goroutine, which blocks on the lock until the caller (this
		// method's caller, holding s.mu) releases it.
		go s.dispatch(name)
		return
	}
	// Still in the future: on the injected clock, so a test advances time to
	// fire it rather than waiting out the delay.
	q.timers[name] = s.clk.AfterFunc(delay, func() { s.dispatch(name) })
}

// dispatch sends a due task and applies the retry policy. It runs off a timer,
// so it takes the lock itself.
func (s *Service) dispatch(name string) {
	ctx := context.Background()

	s.mu.Lock()
	q := s.queues[queueOf(name)]
	if q == nil || q.paused {
		s.mu.Unlock()
		return
	}
	delete(q.timers, name)
	task, err := s.getTaskRecord(ctx, name)
	if err != nil {
		s.mu.Unlock() // the task was deleted before its timer fired
		return
	}
	retry := s.retryConfig(ctx, name)
	s.mu.Unlock()

	code, derr := s.http.do(ctx, task)
	ok := derr == nil && code >= 200 && code < 300

	s.mu.Lock()
	defer s.mu.Unlock()

	// The task may have been deleted or purged while its request was in
	// flight; if so, leave it gone.
	if _, err := s.getTaskRecord(ctx, name); err != nil {
		return
	}

	task.DispatchCount++
	task.LastAttempt = &cloudtaskspb.Attempt{
		DispatchTime: timestamppb.New(s.clk.Now()),
	}
	if ok {
		// A dispatched task is done: Cloud Tasks deletes it on 2xx.
		_ = s.kv.Delete(ctx, taskKey(name), 0)
		return
	}

	if int(task.DispatchCount) >= retry.maxAttempts {
		// Out of attempts: drop it, as Cloud Tasks does.
		_ = s.kv.Delete(ctx, taskKey(name), 0)
		return
	}

	// Back off and try again. Doubling per attempt, capped.
	backoff := retry.minBackoff << (task.DispatchCount - 1)
	if backoff > retry.maxBackoff {
		backoff = retry.maxBackoff
	}
	next := s.clk.Now().Add(backoff)
	task.ScheduleTime = timestamppb.New(next)
	if err := s.putTask(ctx, task); err == nil {
		s.schedule(name, next)
	}
}

type retryPolicy struct {
	maxAttempts int
	minBackoff  time.Duration
	maxBackoff  time.Duration
}

// retryConfig reads the queue's retry policy, filling defaults. The caller
// holds s.mu.
func (s *Service) retryConfig(ctx context.Context, taskName string) retryPolicy {
	p := retryPolicy{defaultMaxAttempts, defaultMinBackoff, defaultMaxBackoff}

	q, err := s.getQueueRecord(ctx, queueOf(taskName))
	if err != nil {
		return p
	}
	rc := q.GetRetryConfig()
	if rc == nil {
		return p
	}
	if rc.MaxAttempts != 0 {
		// -1 means unlimited in the API; a local run needs a ceiling, so an
		// unlimited policy is treated as the default cap.
		if rc.MaxAttempts > 0 {
			p.maxAttempts = int(rc.MaxAttempts)
		}
	}
	if d := rc.GetMinBackoff().AsDuration(); d > 0 {
		p.minBackoff = d
	}
	if d := rc.GetMaxBackoff().AsDuration(); d > 0 {
		p.maxBackoff = d
	}
	return p
}

func (s *Service) putTask(ctx context.Context, t *cloudtaskspb.Task) error {
	encoded, err := marshal.Marshal(t)
	if err != nil {
		return err
	}
	_, current, err := s.kv.Get(ctx, taskKey(t.GetName()))
	var ifVersion uint64
	if err == nil {
		ifVersion = current
	}
	_, err = s.kv.Put(ctx, taskKey(t.GetName()), encoded, ifVersion)
	return err
}

// queueOf returns the queue name a task belongs to: the task name without its
// /tasks/{id} tail.
func queueOf(taskName string) string {
	if i := indexOfTasks(taskName); i >= 0 {
		return taskName[:i]
	}
	return taskName
}

func indexOfTasks(name string) int {
	const marker = "/tasks/"
	for i := 0; i+len(marker) <= len(name); i++ {
		if name[i:i+len(marker)] == marker {
			return i
		}
	}
	return -1
}

// realDoer sends a task's HTTP request for real.
type realDoer struct{}

func (realDoer) do(ctx context.Context, t *cloudtaskspb.Task) (int, error) {
	h := t.GetHttpRequest()
	if h == nil {
		// App Engine targets are not supported; nothing to send.
		return 0, errNoHTTPTarget
	}

	method := h.GetHttpMethod().String()
	if method == "HTTP_METHOD_UNSPECIFIED" || method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, h.GetUrl(), bytes.NewReader(h.GetBody()))
	if err != nil {
		return 0, err
	}
	for k, v := range h.GetHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
