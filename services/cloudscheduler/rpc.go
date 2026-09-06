package cloudscheduler

import (
	"context"
	"sort"
	"strings"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Service) CreateJob(ctx context.Context, req *schedulerpb.CreateJobRequest) (*schedulerpb.Job, error) {
	job := req.GetJob()
	if job == nil || job.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "job.name is required")
	}
	if err := validJobName(job.GetName()); err != nil {
		return nil, err
	}
	// The name must live under the requested parent. Otherwise the job would
	// be stored and scheduled under a project/location the caller did not name
	// — absent from that parent's ListJobs and running in the wrong scope.
	if got := jobCollection(job.GetName()); got != req.GetParent() {
		return nil, status.Errorf(codes.InvalidArgument,
			"job name %q is not under the parent %q", job.GetName(), req.GetParent())
	}
	if _, err := cronParser.Parse(job.GetSchedule()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid schedule %q: %v", job.GetSchedule(), err)
	}
	if job.GetTarget() == nil {
		return nil, status.Error(codes.InvalidArgument, "a job needs a target")
	}

	job.State = schedulerpb.Job_ENABLED
	job.UserUpdateTime = timestamppb.New(s.clk.Now())

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, _, err := s.kv.Get(ctx, jobKey(job.GetName())); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "Job already exists: %s", job.GetName())
	}
	if err := s.putJob(job); err != nil {
		return nil, err
	}
	s.arm(job, s.clk.Now())
	return job, nil
}

// jobCollection returns the parent a job name belongs to: the name without its
// /jobs/{id} tail.
func jobCollection(name string) string {
	const marker = "/jobs/"
	if i := strings.Index(name, marker); i >= 0 {
		return name[:i]
	}
	return name
}

func (s *Service) GetJob(ctx context.Context, req *schedulerpb.GetJobRequest) (*schedulerpb.Job, error) {
	return s.getJobRecord(req.GetName())
}

func (s *Service) ListJobs(ctx context.Context, req *schedulerpb.ListJobsRequest) (*schedulerpb.ListJobsResponse, error) {
	entries, _, err := s.kv.List(ctx, jobPrefix(req.GetParent()), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing jobs: %v", err)
	}
	out := make([]*schedulerpb.Job, 0, len(entries))
	for _, kv := range entries {
		var j schedulerpb.Job
		if err := unmarshal.Unmarshal(kv.Val, &j); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding job: %v", err)
		}
		out = append(out, &j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &schedulerpb.ListJobsResponse{Jobs: out}, nil
}

func (s *Service) DeleteJob(ctx context.Context, req *schedulerpb.DeleteJobRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.kv.Delete(ctx, jobKey(req.GetName()), 0); err != nil {
		return nil, status.Errorf(codes.NotFound, "Job not found: %s", req.GetName())
	}
	s.stopTimer(req.GetName())
	return &emptypb.Empty{}, nil
}

func (s *Service) PauseJob(ctx context.Context, req *schedulerpb.PauseJobRequest) (*schedulerpb.Job, error) {
	return s.setState(req.GetName(), schedulerpb.Job_PAUSED)
}

func (s *Service) ResumeJob(ctx context.Context, req *schedulerpb.ResumeJobRequest) (*schedulerpb.Job, error) {
	return s.setState(req.GetName(), schedulerpb.Job_ENABLED)
}

// setState pauses or resumes a job. Pausing cancels its pending fire; resuming
// arms it for the next cron time, so a job paused across its window fires once
// when it comes back rather than catching up every missed slot.
func (s *Service) setState(name string, state schedulerpb.Job_State) (*schedulerpb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, err := s.getJobRecord(name)
	if err != nil {
		return nil, err
	}
	job.State = state
	if err := s.putJob(job); err != nil {
		return nil, err
	}
	s.stopTimer(name)
	if state == schedulerpb.Job_ENABLED {
		s.arm(job, s.clk.Now())
	}
	return job, nil
}

// RunJob fires a job now, ahead of its schedule, without disturbing its next
// scheduled fire.
func (s *Service) RunJob(ctx context.Context, req *schedulerpb.RunJobRequest) (*schedulerpb.Job, error) {
	s.mu.Lock()
	job, err := s.getJobRecord(req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	s.deliver(job)
	return job, nil
}

// UpdateJob replaces the mutable fields and re-arms if the schedule changed.
func (s *Service) UpdateJob(ctx context.Context, req *schedulerpb.UpdateJobRequest) (*schedulerpb.Job, error) {
	incoming := req.GetJob()
	if incoming == nil || incoming.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "job.name is required")
	}
	if sched := incoming.GetSchedule(); sched != "" {
		if _, err := cronParser.Parse(sched); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid schedule %q: %v", sched, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.getJobRecord(incoming.GetName())
	if err != nil {
		return nil, err
	}
	// The paths the client actually updates; a full field-mask engine is more
	// than a local emulator needs.
	if incoming.GetSchedule() != "" {
		current.Schedule = incoming.GetSchedule()
	}
	if incoming.GetTarget() != nil {
		current.Target = incoming.GetTarget()
	}
	if incoming.GetTimeZone() != "" {
		current.TimeZone = incoming.GetTimeZone()
	}
	current.UserUpdateTime = timestamppb.New(s.clk.Now())

	if err := s.putJob(current); err != nil {
		return nil, err
	}
	s.stopTimer(current.GetName())
	s.arm(current, s.clk.Now())
	return current, nil
}
