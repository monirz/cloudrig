package cloudscheduler

import (
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MaxBodyBytes caps a JSON request body; the port is shared, so an unbounded
// read is a way to exhaust the process.
const MaxBodyBytes = 4 << 20

// REST serves the Cloud Scheduler JSON API so `gcloud scheduler` works. The Go
// client speaks gRPC; gcloud speaks REST, over the same service.
type REST struct {
	router *transport.Router
	svc    *Service
}

// NewREST wires the routes gcloud drives.
func NewREST(s *Service) *REST {
	a := &REST{router: transport.NewRouter(), svc: s}

	const jobs = "/v1/projects/{project}/locations/{location}/jobs"
	a.router.Handle(http.MethodGet, jobs, a.listJobs)
	a.router.Handle(http.MethodPost, jobs, a.createJob)
	a.router.Handle(http.MethodGet, jobs+"/{job}", a.getJob)
	a.router.Handle(http.MethodPatch, jobs+"/{job}", a.updateJob)
	a.router.Handle(http.MethodPost, jobs+"/{job}", a.jobVerb)
	a.router.Handle(http.MethodDelete, jobs+"/{job}", a.deleteJob)
	return a
}

// Matches reports whether a route here claims the request; /v1/ is shared.
func (a *REST) Matches(method, escapedPath string) bool {
	return a.router.Matches(method, escapedPath)
}

func (a *REST) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

func jobName(p transport.Params) string {
	return "projects/" + p["project"] + "/locations/" + p["location"] + "/jobs/" + p["job"]
}

func parent(p transport.Params) string {
	return "projects/" + p["project"] + "/locations/" + p["location"]
}

func (a *REST) createJob(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var job schedulerpb.Job
	if err := decode(r, &job); err != nil {
		return err
	}
	out, err := a.svc.CreateJob(r.Context(), &schedulerpb.CreateJobRequest{
		Parent: parent(p), Job: &job,
	})
	return respond(w, out, err)
}

func (a *REST) getJob(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.GetJob(r.Context(), &schedulerpb.GetJobRequest{Name: jobName(p)})
	return respond(w, out, err)
}

func (a *REST) listJobs(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.ListJobs(r.Context(), &schedulerpb.ListJobsRequest{Parent: parent(p)})
	return respond(w, out, err)
}

func (a *REST) updateJob(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var job schedulerpb.Job
	if err := decode(r, &job); err != nil {
		return err
	}
	job.Name = jobName(p)
	out, err := a.svc.UpdateJob(r.Context(), &schedulerpb.UpdateJobRequest{Job: &job})
	return respond(w, out, err)
}

func (a *REST) deleteJob(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.DeleteJob(r.Context(), &schedulerpb.DeleteJobRequest{Name: jobName(p)})
	return respond(w, out, err)
}

// jobVerb answers :pause, :resume and :run, which ride on the name segment.
func (a *REST) jobVerb(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb, ok := strings.Cut(p["job"], ":")
	if !ok {
		return unsupportedVerb(p["job"], "")
	}
	full := parent(p) + "/jobs/" + name

	ctx := r.Context()
	switch verb {
	case "pause":
		out, err := a.svc.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: full})
		return respond(w, out, err)
	case "resume":
		out, err := a.svc.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: full})
		return respond(w, out, err)
	case "run":
		out, err := a.svc.RunJob(ctx, &schedulerpb.RunJobRequest{Name: full})
		return respond(w, out, err)
	}
	return unsupportedVerb(full, verb)
}

func unsupportedVerb(name, verb string) error {
	return gerr.New(gerr.Unimplemented, "unsupported verb "+verb+" on "+name).
		WithHTTPStatus(http.StatusNotImplemented)
}

func decode(r *http.Request, into proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return gerr.New(gerr.InvalidArgument, "reading the request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if len(body) > MaxBodyBytes {
		return gerr.New(gerr.InvalidArgument, "the request body is too large").
			WithHTTPStatus(http.StatusRequestEntityTooLarge)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := unmarshal.Unmarshal(body, into); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed JSON body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	return nil
}

func respond(w http.ResponseWriter, out proto.Message, err error) error {
	if err != nil {
		st, _ := status.FromError(err)
		return gerr.New(gerr.Code(st.Code()), st.Message()).WithHTTPStatus(httpStatusOf(st.Code()))
	}
	body, marshalErr := marshal.Marshal(out)
	if marshalErr != nil {
		return gerr.New(gerr.Internal, "encoding the response: "+marshalErr.Error())
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if len(body) == 0 {
		body = []byte("{}")
	}
	_, _ = w.Write(append(body, '\n'))
	return nil
}

func httpStatusOf(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.Unimplemented:
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}
