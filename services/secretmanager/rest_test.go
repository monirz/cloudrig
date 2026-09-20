package secretmanager

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// TestRESTWalk drives the JSON surface gcloud uses, in order, over one secret
// and its first version.
func TestRESTWalk(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewREST(New(store.NewMemory(), clock.NewFake(epoch))))
	t.Cleanup(srv.Close)

	const s = "/v1/projects/p/secrets"
	const k = s + "/k"
	const auto = `{"replication":{"automatic":{}}}`
	for _, st := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", s + "?secretId=k", auto, 200},
		{"POST", s + "?secretId=k", auto, 409},
		{"POST", s + "?secretId=k2", `{`, 400},
		{"GET", s, ``, 200},
		{"GET", k, ``, 200},
		{"GET", k + ":getIamPolicy", ``, 200},
		{"GET", k + ":bogus", ``, 501},
		{"GET", s + "/missing", ``, 404},

		{"POST", k + ":setIamPolicy", `{}`, 200},
		{"POST", k + ":testIamPermissions", `{"permissions":["secretmanager.secrets.get"]}`, 200},
		{"POST", k, `{}`, 501},
		{"POST", k + ":bogus", `{}`, 501},
		{"POST", k + ":addVersion", `{`, 400},
		{"POST", k + ":addVersion", `{"payload":{"data":"aGk="}}`, 200},
		{"POST", s + "/missing:addVersion", `{"payload":{"data":"aGk="}}`, 404},

		{"PATCH", k + "?updateMask=labels", `{"labels":{"team":"core"}}`, 200},
		{"PATCH", k + "?updateMask=labels", `{`, 400},
		{"PATCH", s + "/missing?updateMask=labels", `{"labels":{"a":"b"}}`, 404},

		{"GET", k + "/versions", ``, 200},
		{"GET", k + "/versions/1", ``, 200},
		{"GET", k + "/versions/latest:access", ``, 200},
		{"GET", k + "/versions/9", ``, 404},
		{"GET", k + "/versions/1:bogus", ``, 501},

		{"POST", k + "/versions/1:disable", ``, 200},
		{"GET", k + "/versions/1:access", ``, 400},
		{"POST", k + "/versions/1:enable", ``, 200},
		{"POST", k + "/versions/1:destroy", ``, 200},
		{"POST", k + "/versions/9:disable", ``, 404},
		{"POST", k + "/versions/1", ``, 501},
		{"POST", k + "/versions/1:bogus", ``, 501},

		{"DELETE", k, ``, 200},
		{"DELETE", k, ``, 404},
	} {
		req, _ := http.NewRequest(st.method, srv.URL+st.path, strings.NewReader(st.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != st.want {
			t.Errorf("%s %s = %d, want %d: %s", st.method, st.path, resp.StatusCode, st.want, body)
		}
	}
}

func TestRESTRefusesAnOversizedBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewREST(New(store.NewMemory(), clock.NewFake(epoch))))
	t.Cleanup(srv.Close)

	big := strings.NewReader(strings.Repeat(" ", MaxBodyBytes+1))
	resp, err := http.Post(srv.URL+"/v1/projects/p/secrets/k:testIamPermissions", "application/json", big)
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
		codes.OK:               200,
		codes.OutOfRange:       400,
		codes.Aborted:          409,
		codes.PermissionDenied: 403,
		codes.Unimplemented:    501,
		codes.Unavailable:      500,
	} {
		if got := httpStatusOf(code); got != want {
			t.Errorf("httpStatusOf(%v) = %d, want %d", code, got, want)
		}
	}
}
