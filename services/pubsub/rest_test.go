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

// TestRESTPublishPullAck is the round trip gcloud topics publish and
// subscriptions pull --auto-ack make.
func TestRESTPublishPullAck(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewREST(newService(t)))
	t.Cleanup(srv.Close)

	do := func(method, path, body string, want int) []byte {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, out)
		}
		return out
	}

	do("PUT", "/v1/projects/p/topics/t", ``, 200)
	do("PUT", "/v1/projects/p/subscriptions/s", `{"topic":"projects/p/topics/t"}`, 200)
	do("POST", "/v1/projects/p/topics/t:publish", `{"messages":[{"data":"aGk="}]}`, 200) // "hi"

	var pulled struct {
		ReceivedMessages []struct {
			AckID   string `json:"ackId"`
			Message struct{ Data string }
		} `json:"receivedMessages"`
	}
	body := do("POST", "/v1/projects/p/subscriptions/s:pull", `{"maxMessages":10}`, 200)
	if err := json.Unmarshal(body, &pulled); err != nil {
		t.Fatal(err)
	}
	if len(pulled.ReceivedMessages) != 1 || pulled.ReceivedMessages[0].Message.Data != "aGk=" {
		t.Fatalf("pull = %s", body)
	}

	ack, _ := json.Marshal(map[string]any{"ackIds": []string{pulled.ReceivedMessages[0].AckID}})
	do("POST", "/v1/projects/p/subscriptions/s:acknowledge", string(ack), 200)
	if body := do("POST", "/v1/projects/p/subscriptions/s:pull", `{"maxMessages":10}`, 200); strings.Contains(string(body), "ackId") {
		t.Errorf("an acknowledged message came back: %s", body)
	}

	do("POST", "/v1/projects/p/topics/missing:publish", `{"messages":[{"data":"aGk="}]}`, 404)
	do("POST", "/v1/projects/p/topics/t", ``, 400)
	do("POST", "/v1/projects/p/topics/t:frobnicate", ``, 501)
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
