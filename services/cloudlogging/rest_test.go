package cloudlogging

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/core/clock"
)

// TestRESTWriteReadDelete is the round trip gcloud logging write, read and
// logs delete make.
func TestRESTWriteReadDelete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewREST(New(clock.NewFake(epoch))))
	t.Cleanup(srv.Close)

	do := func(method, path, body string, want int) string {
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
		return string(out)
	}

	do("POST", "/v2/entries:write", `{"logName":"projects/p/logs/app","resource":{"type":"global"},
		"entries":[{"textPayload":"hello"},{"severity":"ERROR","jsonPayload":{"order":42}}]}`, 200)

	all := do("POST", "/v2/entries:list", `{"resourceNames":["projects/p"],"filter":"logName:app","orderBy":"timestamp desc"}`, 200)
	if !strings.Contains(all, `"hello"`) || !strings.Contains(all, `"order":42`) {
		t.Errorf("list = %s", all)
	}
	errs := do("POST", "/v2/entries:list", `{"resourceNames":["projects/p"],"filter":"severity>=ERROR"}`, 200)
	if strings.Contains(errs, `"hello"`) || !strings.Contains(errs, `"order":42`) {
		t.Errorf("severity filter = %s", errs)
	}

	if logs := do("GET", "/v2/projects/p/logs", ``, 200); !strings.Contains(logs, "projects/p/logs/app") {
		t.Errorf("logs = %s", logs)
	}
	do("DELETE", "/v2/projects/p/logs/app", ``, 200)
	if left := do("POST", "/v2/entries:list", `{"resourceNames":["projects/p"]}`, 200); strings.Contains(left, "hello") {
		t.Errorf("entries survived the delete: %s", left)
	}

	do("POST", "/v2/entries:list", `{"filter":"nonsense"}`, 400)
	do("POST", "/v2/entries:write", `{`, 400)
}
