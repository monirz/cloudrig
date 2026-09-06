package cloudscheduler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// arm schedules a job's next fire from a base time. The caller holds s.mu.
//
// A paused job arms nothing. A job with an unparseable schedule is left inert
// rather than firing wrongly; CreateJob rejects those up front, so this only
// guards a record that reached the store some other way.
func (s *Service) arm(j *schedulerpb.Job, from time.Time) {
	if j.GetState() == schedulerpb.Job_PAUSED {
		return
	}
	sched, err := cronParser.Parse(j.GetSchedule())
	if err != nil {
		return
	}
	next := sched.Next(from)
	name := j.GetName()
	delay := next.Sub(s.clk.Now())
	if delay < 0 {
		delay = 0
	}
	s.timers[name] = s.clk.AfterFunc(delay, func() { s.fire(name) })
}

// fire runs a job and re-arms it for its next cron time. It runs off a timer,
// so it takes the lock itself.
func (s *Service) fire(name string) {
	s.mu.Lock()
	job, err := s.getJobRecord(name)
	if err != nil || job.GetState() == schedulerpb.Job_PAUSED {
		s.mu.Unlock()
		return
	}
	firedAt := s.clk.Now()
	job.LastAttemptTime = timestamppb.New(firedAt)
	_ = s.putJob(job)

	// Re-arm before dispatching, from the fire time, so a job that recurs
	// every minute is already scheduled for the next minute regardless of how
	// long the target takes.
	s.arm(job, firedAt)
	s.mu.Unlock()

	s.deliver(job)
}

// deliver sends a job to its target. Pub/Sub targets go through the bus
// synchronously; HTTP targets run on a goroutine so a slow endpoint does not
// hold the fire path, and Sync waits on them.
func (s *Service) deliver(job *schedulerpb.Job) {
	switch t := job.GetTarget().(type) {
	case *schedulerpb.Job_PubsubTarget:
		s.deliverPubsub(t.PubsubTarget)
	case *schedulerpb.Job_HttpTarget:
		s.inFlight.Add(1)
		go func() {
			defer s.inFlight.Done()
			_, _ = s.http.do(job)
		}()
	}
}

// deliverPubsub publishes a real message to the target topic, the same path a
// real Scheduler → Pub/Sub job takes: a subscriber receives it, and Pub/Sub's
// own notify fires any function trigger on the topic.
func (s *Service) deliverPubsub(t *schedulerpb.PubsubTarget) {
	if s.publish == nil {
		return
	}
	_ = s.publish(context.Background(), t.GetTopicName(), t.GetData(), t.GetAttributes())
}

// recover re-arms every stored job, so an emulator built over an existing store
// keeps firing. Not holding s.mu.
func (s *Service) recover() {
	entries, _, err := s.kv.List(context.Background(), "cs/j/", 0, "")
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, kv := range entries {
		var j schedulerpb.Job
		if err := unmarshal.Unmarshal(kv.Val, &j); err != nil {
			continue
		}
		s.arm(&j, s.clk.Now())
	}
}

// stopTimer cancels a job's pending fire. The caller holds s.mu.
func (s *Service) stopTimer(name string) {
	if timer := s.timers[name]; timer != nil {
		timer.Stop()
		delete(s.timers, name)
	}
}

// realDoer sends an HTTP-target job's request.
type realDoer struct{}

func (realDoer) do(job *schedulerpb.Job) (int, error) {
	h, ok := job.GetTarget().(*schedulerpb.Job_HttpTarget)
	if !ok {
		return 0, nil
	}
	t := h.HttpTarget

	method := t.GetHttpMethod().String()
	if method == "HTTP_METHOD_UNSPECIFIED" || method == "" {
		method = http.MethodPost
	}

	// Bounded, so a target that accepts a connection and never answers cannot
	// pile up one stuck goroutine per recurrence and hang SyncScheduler. The
	// job's attempt deadline caps it, defaulting to Cloud Scheduler's 180s.
	deadline := job.GetAttemptDeadline().AsDuration()
	if deadline <= 0 {
		deadline = defaultAttemptDeadline
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, t.GetUri(), bytes.NewReader(t.GetBody()))
	if err != nil {
		return 0, err
	}
	for k, v := range t.GetHeaders() {
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

// defaultAttemptDeadline is Cloud Scheduler's default when a job sets none.
const defaultAttemptDeadline = 180 * time.Second
