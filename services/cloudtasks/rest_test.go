package cloudtasks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

const (
	restQueues = "/v2/projects/p/locations/us-central1/queues"
	restQueue  = restQueues + "/q"
)

func newREST(t *testing.T) *REST {
	t.Helper()
	return NewREST(New(store.NewMemory(), clock.NewFake(epoch)))
}

// do drives one request through the router, as gcloud would.
func do(t *testing.T, a *REST, method, path, body string) (int, string) {
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

func mustCreateQueue(t *testing.T, a *REST) {
	t.Helper()
	if code, body := do(t, a, http.MethodPost, restQueues+"?queueId=q", `{}`); code != http.StatusOK {
		t.Fatalf("creating the queue: %d %s", code, body)
	}
}

// The whole queue surface gcloud drives, in the order it drives it.
func TestRESTQueueLifecycle(t *testing.T) {
	t.Parallel()
	a := newREST(t)

	mustCreateQueue(t, a)

	code, body := do(t, a, http.MethodGet, restQueue, "")
	if code != http.StatusOK || !strings.Contains(body, "/queues/q") {
		t.Errorf("get = %d %s", code, body)
	}

	code, body = do(t, a, http.MethodGet, restQueues, "")
	if code != http.StatusOK || !strings.Contains(body, "/queues/q") {
		t.Errorf("list = %d %s", code, body)
	}

	for _, verb := range []string{"pause", "resume", "purge"} {
		if code, body := do(t, a, http.MethodPost, restQueue+":"+verb, ""); code != http.StatusOK {
			t.Errorf("%s = %d %s", verb, code, body)
		}
	}

	if code, body := do(t, a, http.MethodDelete, restQueue, ""); code != http.StatusOK {
		t.Errorf("delete = %d %s", code, body)
	}
	if code, _ := do(t, a, http.MethodGet, restQueue, ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// A queue named in the body rather than with ?queueId=.
func TestRESTCreateQueueFromTheBody(t *testing.T) {
	t.Parallel()
	a := newREST(t)

	body := `{"name":"projects/p/locations/us-central1/queues/fromBody"}`
	if code, got := do(t, a, http.MethodPost, restQueues, body); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, got)
	}
	if code, _ := do(t, a, http.MethodGet, restQueues+"/fromBody", ""); code != http.StatusOK {
		t.Errorf("the queue named in the body was not created")
	}
}

func TestRESTTaskLifecycle(t *testing.T) {
	t.Parallel()
	a := newREST(t)
	mustCreateQueue(t, a)

	const task = `{"task":{"httpRequest":{"url":"http://127.0.0.1:1/","httpMethod":"POST"},` +
		`"scheduleTime":"2030-01-01T00:00:00Z","name":"` +
		`projects/p/locations/us-central1/queues/q/tasks/t1"}}`
	code, body := do(t, a, http.MethodPost, restQueue+"/tasks", task)
	if code != http.StatusOK {
		t.Fatalf("create task = %d %s", code, body)
	}

	code, body = do(t, a, http.MethodGet, restQueue+"/tasks/t1", "")
	if code != http.StatusOK || !strings.Contains(body, "/tasks/t1") {
		t.Errorf("get task = %d %s", code, body)
	}

	code, body = do(t, a, http.MethodGet, restQueue+"/tasks", "")
	if code != http.StatusOK || !strings.Contains(body, "/tasks/t1") {
		t.Errorf("list tasks = %d %s", code, body)
	}

	if code, body := do(t, a, http.MethodDelete, restQueue+"/tasks/t1", ""); code != http.StatusOK {
		t.Errorf("delete task = %d %s", code, body)
	}
	if code, _ := do(t, a, http.MethodGet, restQueue+"/tasks/t1", ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// :run is reachable both as a POST verb and on the GET route, because gcloud
// has used both.
func TestRESTRunTask(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			a := newREST(t)
			mustCreateQueue(t, a)

			sink := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			t.Cleanup(sink.Close)

			body := `{"task":{"httpRequest":{"url":"` + sink.URL + `","httpMethod":"POST"},` +
				`"scheduleTime":"2030-01-01T00:00:00Z"}}`
			if code, got := do(t, a, http.MethodPost, restQueue+"/tasks", body); code != http.StatusOK {
				t.Fatalf("create task = %d %s", code, got)
			}
			name := taskNameFrom(t, a)

			if code, got := do(t, a, method, restQueue+"/tasks/"+name+":run", ""); code != http.StatusOK {
				t.Errorf("run = %d %s", code, got)
			}
		})
	}
}

// taskNameFrom reads back the single task's short name; the service assigns it.
func taskNameFrom(t *testing.T, a *REST) string {
	t.Helper()
	_, body := do(t, a, http.MethodGet, restQueue+"/tasks", "")
	var got struct {
		Tasks []struct {
			Name string `json:"name"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding the task list: %v (%s)", err, body)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("want one task, got %s", body)
	}
	i := strings.LastIndex(got.Tasks[0].Name, "/")
	return got.Tasks[0].Name[i+1:]
}

// A verb the emulator does not implement says so, rather than being mistaken
// for a resource name.
func TestRESTUnsupportedVerbs(t *testing.T) {
	t.Parallel()
	a := newREST(t)
	mustCreateQueue(t, a)

	tests := []struct {
		name, method, path string
	}{
		{"queue verb", http.MethodPost, restQueue + ":teleport"},
		{"queue with no verb at all", http.MethodPost, restQueue},
		{"task verb", http.MethodPost, restQueue + "/tasks/t1:teleport"},
		{"task verb on GET", http.MethodGet, restQueue + "/tasks/t1:teleport"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, body := do(t, a, tc.method, tc.path, "")
			if code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501 (%s)", code, body)
			}
		})
	}
}

func TestRESTRejectsBadBodies(t *testing.T) {
	t.Parallel()

	t.Run("malformed JSON", func(t *testing.T) {
		t.Parallel()
		a := newREST(t)
		code, body := do(t, a, http.MethodPost, restQueues+"?queueId=q", `{"name":`)
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (%s)", code, body)
		}
	})

	t.Run("a body over the cap", func(t *testing.T) {
		t.Parallel()
		a := newREST(t)
		huge := `{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
		code, body := do(t, a, http.MethodPost, restQueues+"?queueId=q", huge)
		if code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413 (%s)", code, body)
		}
	})

	t.Run("an empty body is not an error", func(t *testing.T) {
		t.Parallel()
		a := newREST(t)
		if code, body := do(t, a, http.MethodPost, restQueues+"?queueId=q", ""); code != http.StatusOK {
			t.Errorf("status = %d, want 200 (%s)", code, body)
		}
	})
}

// A gRPC code has one HTTP status gcloud expects to see.
func TestHTTPStatusOf(t *testing.T) {
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
		codes.Unknown:            http.StatusInternalServerError,
	}
	for code, want := range tests {
		if got := httpStatusOf(code); got != want {
			t.Errorf("httpStatusOf(%v) = %d, want %d", code, got, want)
		}
	}
}

// An empty proto still has to render as an object: gcloud parses the body.
func TestRESTEmptyResponseIsAnObject(t *testing.T) {
	t.Parallel()
	a := newREST(t)
	mustCreateQueue(t, a)

	code, body := do(t, a, http.MethodPost, restQueue+":purge", "")
	if code != http.StatusOK {
		t.Fatalf("purge = %d %s", code, body)
	}
	if strings.TrimSpace(body) == "" {
		t.Error("an empty body is not valid JSON for the client")
	}
}

// A route on a queue that is not there is a 404, not a 500.
func TestRESTMissingQueue(t *testing.T) {
	t.Parallel()
	a := newREST(t)

	for _, path := range []string{restQueue, restQueue + "/tasks/t1"} {
		if code, body := do(t, a, http.MethodGet, path, ""); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (%s)", path, code, body)
		}
	}
}

// ListTasks is a prefix scan with no existence check, so an absent queue reads
// as an empty one. Real Cloud Tasks answers 404; see UNSUPPORTED.md.
func TestRESTListTasksOnAMissingQueue(t *testing.T) {
	t.Parallel()
	a := newREST(t)

	code, body := do(t, a, http.MethodGet, restQueue+"/tasks", "")
	if code != http.StatusOK || strings.Contains(body, "tasks") {
		t.Errorf("list = %d %s, want an empty 200", code, body)
	}
}
