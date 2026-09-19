package pubsub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
)

// TestRESTWalk drives the JSON surface Terraform uses, in order, over one
// topic and one subscription.
func TestRESTWalk(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewREST(newService(t)))
	t.Cleanup(srv.Close)

	const topics = "/v1/projects/p/topics"
	const subs = "/v1/projects/p/subscriptions"
	for _, s := range []struct {
		method, path, body string
		want               int
	}{
		{"PUT", topics + "/t", ``, 200},
		{"PUT", topics + "/t", ``, 409},
		{"PUT", topics + "/u", `{`, 400},
		{"GET", topics, ``, 200},
		{"PATCH", topics + "/t?updateMask=labels", `{"topic":{"labels":{"k":"v"}}}`, 200},
		{"PATCH", topics + "/t", `{`, 400},

		{"PUT", subs + "/s", `{"topic":"projects/p/topics/t"}`, 200},
		{"PUT", subs + "/s", `{"topic":"projects/p/topics/t"}`, 409},
		{"PUT", subs + "/s2", `{"topic":"projects/p/topics/missing"}`, 404},
		{"PUT", subs + "/s2", `{"topic":"bad name"}`, 400},
		{"PUT", subs + "/s2", `{`, 400},
		{"GET", subs + "/s", ``, 200},
		{"GET", subs, ``, 200},
		{"PATCH", subs + "/s?updateMask=ackDeadlineSeconds", `{"subscription":{"ackDeadlineSeconds":30}}`, 200},
		{"PATCH", subs + "/s", ``, 400}, // no update mask
		{"PATCH", subs + "/s", `{`, 400},
		{"PATCH", subs + "/missing?updateMask=ackDeadlineSeconds", `{"subscription":{"ackDeadlineSeconds":30}}`, 404},
		{"DELETE", subs + "/s", ``, 200},
		{"DELETE", subs + "/s", ``, 404},

		{"DELETE", topics + "/t", ``, 200},
		{"DELETE", topics + "/t", ``, 404},
	} {
		req, _ := http.NewRequest(s.method, srv.URL+s.path, strings.NewReader(s.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != s.want {
			t.Errorf("%s %s = %d, want %d: %s", s.method, s.path, resp.StatusCode, s.want, body)
		}
	}
}

func TestRESTListsWhatWasCreated(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewREST(newService(t)))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/v1/projects/p/topics/a", "/v1/projects/p/topics/b"} {
		req, _ := http.NewRequest("PUT", srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	resp, err := http.Get(srv.URL + "/v1/projects/p/topics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Topics []struct{ Name string } }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Topics) != 2 || out.Topics[0].Name != "projects/p/topics/a" {
		t.Errorf("topics = %+v", out.Topics)
	}
}

func TestRESTRefusesAnOversizedBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewREST(newService(t)))
	t.Cleanup(srv.Close)

	big := strings.NewReader(strings.Repeat(" ", MaxBodyBytes+1))
	req, _ := http.NewRequest("PUT", srv.URL+"/v1/projects/p/topics/t", big)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestHTTPStatusOf(t *testing.T) {
	t.Parallel()
	for code, want := range map[codes.Code]int{
		codes.OK:                 200,
		codes.OutOfRange:         400,
		codes.Aborted:            409,
		codes.PermissionDenied:   403,
		codes.Unauthenticated:    401,
		codes.Unimplemented:      501,
		codes.ResourceExhausted:  500,
		codes.FailedPrecondition: 400,
	} {
		if got := httpStatusOf(code); got != want {
			t.Errorf("httpStatusOf(%v) = %d, want %d", code, got, want)
		}
	}
}
