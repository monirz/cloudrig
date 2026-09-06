// Package cloudscheduler is the Cloud Scheduler emulation.
//
// A job is a cron expression and a target: run this URL, or publish to this
// topic, on this schedule. Unlike a Cloud Task, a job recurs — after each fire
// it re-arms for its next cron time. The whole cycle runs on the injected
// clock, so a daily job fires twenty-four times when a test advances a day,
// rather than once a day in real time. That is the thing a wall-clock emulator
// cannot do.
package cloudscheduler

import (
	"context"
	"strings"
	"sync"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"github.com/robfig/cron/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// cronParser accepts the standard five-field expression Cloud Scheduler uses.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Protos are stored as protojson: a job's target is a oneof no plain JSON
// decoder can rebuild.
var (
	marshal   = protojson.MarshalOptions{}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// Service holds jobs and fires them on schedule.
type Service struct {
	schedulerpb.UnimplementedCloudSchedulerServer

	kv   store.Store
	clk  clock.Clock
	http httpDoer

	// publish delivers a Pub/Sub-target job's message. It is the Pub/Sub
	// service's own Publish, so the message is a real one a subscriber
	// receives — not just a bus event — and Pub/Sub's own notify still fires
	// the trigger for any function watching the topic.
	publish PublishFunc

	mu       sync.Mutex
	timers   map[string]clock.Timer // job name -> its next fire
	inFlight sync.WaitGroup
}

// httpDoer sends an HTTP-target job's request; an interface so a test can
// observe fires without a server.
type httpDoer interface {
	do(job *schedulerpb.Job) (int, error)
}

// PublishFunc delivers a message to a Pub/Sub topic.
type PublishFunc func(ctx context.Context, topic string, data []byte, attrs map[string]string) error

// New wires a service and re-arms any jobs the store already holds. publish may
// be nil, in which case Pub/Sub-target jobs are dropped.
func New(kv store.Store, clk clock.Clock, publish PublishFunc) *Service {
	s := &Service{
		kv:      kv,
		clk:     clk,
		http:    realDoer{},
		publish: publish,
		timers:  map[string]clock.Timer{},
	}
	s.recover()
	return s
}

// Sync waits for in-flight HTTP fires. A test advancing the clock across a
// job's time uses this before asserting, since an HTTP target dispatches on a
// goroutine.
func (s *Service) Sync() { s.inFlight.Wait() }

func jobKey(name string) string { return "cs/j/" + name }

func jobPrefix(parent string) string { return "cs/j/" + parent + "/jobs/" }

// validJobName checks projects/{p}/locations/{l}/jobs/{j}.
func validJobName(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" ||
		parts[4] != "jobs" || parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return status.Errorf(codes.InvalidArgument,
			"invalid job name %q; expected projects/{p}/locations/{l}/jobs/{j}", name)
	}
	return nil
}

func (s *Service) getJobRecord(name string) (*schedulerpb.Job, error) {
	raw, _, err := s.kv.Get(context.Background(), jobKey(name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Job not found: %s", name)
	}
	var j schedulerpb.Job
	if err := unmarshal.Unmarshal(raw, &j); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding job: %v", err)
	}
	return &j, nil
}

func (s *Service) putJob(j *schedulerpb.Job) error {
	encoded, err := marshal.Marshal(j)
	if err != nil {
		return status.Errorf(codes.Internal, "encoding job: %v", err)
	}
	_, v, err := s.kv.Get(context.Background(), jobKey(j.GetName()))
	var ifVersion uint64
	if err == nil {
		ifVersion = v
	}
	if _, err := s.kv.Put(context.Background(), jobKey(j.GetName()), encoded, ifVersion); err != nil {
		return status.Errorf(codes.Internal, "storing job: %v", err)
	}
	return nil
}
