package cloudscheduler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

const (
	restJobs = "/v1/projects/p/locations/l/jobs"
	restJob  = restJobs + "/j"
	jobFull  = "projects/p/locations/l/jobs/j"
)

func newSched(t *testing.T) *Service {
	t.Helper()
	return New(store.NewMemory(), clock.NewFake(epoch), nil)
}

// call drives one request through the router, as gcloud would.
func call(t *testing.T, a *REST, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if !a.Matches(method, req.URL.EscapedPath()) {
		t.Fatalf("no route claims %s %s", method, path)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// sink is an HTTP target a job can actually reach.
func sink(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	return srv
}

func jobBody(url string) string {
	return `{"name":"` + jobFull + `","schedule":"0 * * * *",` +
		`"httpTarget":{"uri":"` + url + `","httpMethod":"POST"}}`
}

func mustCreateJob(t *testing.T, a *REST, url string) {
	t.Helper()
	if code, body := call(t, a, http.MethodPost, restJobs, jobBody(url)); code != http.StatusOK {
		t.Fatalf("creating the job: %d %s", code, body)
	}
}

// The whole job surface gcloud drives, in the order it drives it.
func TestRESTJobLifecycle(t *testing.T) {
	t.Parallel()
	a := NewREST(newSched(t))
	srv := sink(t)

	mustCreateJob(t, a, srv.URL)

	code, body := call(t, a, http.MethodGet, restJob, "")
	if code != http.StatusOK || !strings.Contains(body, "/jobs/j") {
		t.Errorf("get = %d %s", code, body)
	}

	code, body = call(t, a, http.MethodGet, restJobs, "")
	if code != http.StatusOK || !strings.Contains(body, "/jobs/j") {
		t.Errorf("list = %d %s", code, body)
	}

	patch := `{"schedule":"*/5 * * * *"}`
	code, body = call(t, a, http.MethodPatch, restJob, patch)
	if code != http.StatusOK || !strings.Contains(body, "*/5 * * * *") {
		t.Errorf("patch = %d %s", code, body)
	}

	for _, verb := range []string{"pause", "resume", "run"} {
		if code, body := call(t, a, http.MethodPost, restJob+":"+verb, ""); code != http.StatusOK {
			t.Errorf("%s = %d %s", verb, code, body)
		}
	}

	if code, body := call(t, a, http.MethodDelete, restJob, ""); code != http.StatusOK {
		t.Errorf("delete = %d %s", code, body)
	}
	if code, _ := call(t, a, http.MethodGet, restJob, ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// Pausing takes a job out of service; resuming puts it back, and the state is
// what a reader sees.
func TestRESTPauseShowsInTheJob(t *testing.T) {
	t.Parallel()
	a := NewREST(newSched(t))
	mustCreateJob(t, a, sink(t).URL)

	if code, body := call(t, a, http.MethodPost, restJob+":pause", ""); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	if _, body := call(t, a, http.MethodGet, restJob, ""); !strings.Contains(body, "PAUSED") {
		t.Errorf("state after pause = %s", body)
	}
	if code, body := call(t, a, http.MethodPost, restJob+":resume", ""); code != http.StatusOK {
		t.Fatalf("resume = %d %s", code, body)
	}
	if _, body := call(t, a, http.MethodGet, restJob, ""); !strings.Contains(body, "ENABLED") {
		t.Errorf("state after resume = %s", body)
	}
}

func TestRESTUnsupportedVerbs(t *testing.T) {
	t.Parallel()
	a := NewREST(newSched(t))
	mustCreateJob(t, a, sink(t).URL)

	for _, path := range []string{restJob + ":teleport", restJob} {
		if code, body := call(t, a, http.MethodPost, path, ""); code != http.StatusNotImplemented {
			t.Errorf("POST %s = %d, want 501 (%s)", path, code, body)
		}
	}
}

func TestRESTRejectsBadBodies(t *testing.T) {
	t.Parallel()

	t.Run("malformed JSON", func(t *testing.T) {
		t.Parallel()
		a := NewREST(newSched(t))
		code, body := call(t, a, http.MethodPost, restJobs, `{"name":`)
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (%s)", code, body)
		}
	})

	t.Run("a body over the cap", func(t *testing.T) {
		t.Parallel()
		a := NewREST(newSched(t))
		huge := `{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
		code, body := call(t, a, http.MethodPost, restJobs, huge)
		if code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413 (%s)", code, body)
		}
	})

	t.Run("an empty body is a job with no name", func(t *testing.T) {
		t.Parallel()
		a := NewREST(newSched(t))
		code, body := call(t, a, http.MethodPost, restJobs, "")
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (%s)", code, body)
		}
	})

	t.Run("a patch with a malformed body", func(t *testing.T) {
		t.Parallel()
		a := NewREST(newSched(t))
		code, body := call(t, a, http.MethodPatch, restJob, `{`)
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (%s)", code, body)
		}
	})
}

// A route on a job that is not there is a 404, not a 500.
func TestRESTMissingJob(t *testing.T) {
	t.Parallel()
	a := NewREST(newSched(t))

	if code, body := call(t, a, http.MethodGet, restJob, ""); code != http.StatusNotFound {
		t.Errorf("get = %d, want 404 (%s)", code, body)
	}
	if code, body := call(t, a, http.MethodDelete, restJob, ""); code != http.StatusNotFound {
		t.Errorf("delete = %d, want 404 (%s)", code, body)
	}
	if code, body := call(t, a, http.MethodPatch, restJob, `{"schedule":"0 * * * *"}`); code != http.StatusNotFound {
		t.Errorf("patch = %d, want 404 (%s)", code, body)
	}
}

// A gRPC code has one HTTP status gcloud expects to see.
func TestHTTPStatusOfScheduler(t *testing.T) {
	t.Parallel()

	tests := map[codes.Code]int{
		codes.OK:                 http.StatusOK,
		codes.InvalidArgument:    http.StatusBadRequest,
		codes.FailedPrecondition: http.StatusBadRequest,
		codes.OutOfRange:         http.StatusBadRequest,
		codes.NotFound:           http.StatusNotFound,
		codes.AlreadyExists:      http.StatusConflict,
		codes.Aborted:            http.StatusConflict,
		codes.Unimplemented:      http.StatusNotImplemented,
		codes.Internal:           http.StatusInternalServerError,
		codes.Unavailable:        http.StatusInternalServerError,
	}
	for code, want := range tests {
		if got := httpStatusOf(code); got != want {
			t.Errorf("httpStatusOf(%v) = %d, want %d", code, got, want)
		}
	}
}

func TestCreateJobRejectsBadRequests(t *testing.T) {
	t.Parallel()

	target := &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
		TopicName: "projects/p/topics/t",
	}}
	tests := []struct {
		name   string
		parent string
		job    *schedulerpb.Job
	}{
		{"no job at all", "projects/p/locations/l", nil},
		{"no name", "projects/p/locations/l", &schedulerpb.Job{Schedule: "0 * * * *", Target: target}},
		{"a malformed name", "projects/p/locations/l", &schedulerpb.Job{
			Name: "projects/p/jobs/j", Schedule: "0 * * * *", Target: target,
		}},
		{"a name outside the parent", "projects/p/locations/other", &schedulerpb.Job{
			Name: jobFull, Schedule: "0 * * * *", Target: target,
		}},
		{"an unparseable schedule", "projects/p/locations/l", &schedulerpb.Job{
			Name: jobFull, Schedule: "not a cron", Target: target,
		}},
		{"no target", "projects/p/locations/l", &schedulerpb.Job{
			Name: jobFull, Schedule: "0 * * * *",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSched(t)
			_, err := s.CreateJob(context.Background(), &schedulerpb.CreateJobRequest{
				Parent: tc.parent, Job: tc.job,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("code = %v, want InvalidArgument (err: %v)", status.Code(err), err)
			}
		})
	}
}

func TestCreateJobRefusesADuplicate(t *testing.T) {
	t.Parallel()
	a := NewREST(newSched(t))
	srv := sink(t)

	mustCreateJob(t, a, srv.URL)
	code, body := call(t, a, http.MethodPost, restJobs, jobBody(srv.URL))
	if code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (%s)", code, body)
	}
}

func TestUpdateJobRejectsBadRequests(t *testing.T) {
	t.Parallel()
	s := newSched(t)
	ctx := context.Background()

	if _, err := s.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a nameless update was accepted")
	}
	_, err := s.UpdateJob(ctx, &schedulerpb.UpdateJobRequest{
		Job: &schedulerpb.Job{Name: jobFull, Schedule: "not a cron"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an unparseable schedule was accepted: %v", err)
	}
}

// The target and time zone are the other fields a client patches.
func TestUpdateJobReplacesTargetAndZone(t *testing.T) {
	t.Parallel()
	a := NewREST(newSched(t))
	srv := sink(t)
	mustCreateJob(t, a, srv.URL)

	patch := `{"timeZone":"UTC","pubsubTarget":{"topicName":"projects/p/topics/moved"}}`
	code, body := call(t, a, http.MethodPatch, restJob, patch)
	if code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, body)
	}
	if !strings.Contains(body, "moved") || !strings.Contains(body, "UTC") {
		t.Errorf("the target and zone were not replaced: %s", body)
	}
}

func TestRunJobNeedsTheJob(t *testing.T) {
	t.Parallel()
	s := newSched(t)

	if _, err := s.RunJob(context.Background(), &schedulerpb.RunJobRequest{
		Name: jobFull,
	}); status.Code(err) != codes.NotFound {
		t.Errorf("RunJob on a missing job = %v, want NotFound", err)
	}
	if _, err := s.PauseJob(context.Background(), &schedulerpb.PauseJobRequest{
		Name: jobFull,
	}); status.Code(err) != codes.NotFound {
		t.Errorf("PauseJob on a missing job = %v, want NotFound", err)
	}
}

// failingLists refuses a listing, standing in for a broken store.
type failingLists struct{ store.Store }

func (f failingLists) List(ctx context.Context, prefix string, limit int, token string) ([]store.KV, string, error) {
	return nil, "", errors.New("the store refused")
}

func TestListJobsReportsAStoreFailure(t *testing.T) {
	t.Parallel()

	s := New(failingLists{Store: store.NewMemory()}, clock.NewFake(epoch), nil)
	_, err := s.ListJobs(context.Background(), &schedulerpb.ListJobsRequest{
		Parent: "projects/p/locations/l",
	})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal (err: %v)", status.Code(err), err)
	}
}
