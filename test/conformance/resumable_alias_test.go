package conformance

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/monirz/cloudrig"
)

// gcloud storage cp from stdin opens its session on /resumable/upload/...,
// not /upload/..., then streams chunks with the total unknown until the last.
func TestResumableUploadAlias(t *testing.T) {
	t.Parallel()
	base := cloudrig.MustStart(t).BaseURL()

	do := func(method, url, contentRange, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if contentRange != "" {
			req.Header.Set("Content-Range", contentRange)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	if r := do(http.MethodPost, base+"/storage/v1/b?project=p", "", `{"name":"bkt"}`); r.StatusCode != http.StatusOK {
		t.Fatalf("creating the bucket: %d", r.StatusCode)
	}
	open := do(http.MethodPost, base+"/resumable/upload/storage/v1/b/bkt/o?uploadType=resumable", "", `{"name":"piped.txt"}`)
	session := open.Header.Get("Location")
	if open.StatusCode != http.StatusOK || session == "" {
		t.Fatalf("opening the session: %d, Location %q", open.StatusCode, session)
	}
	if r := do(http.MethodPut, session, "bytes 0-5/*", "hello "); r.StatusCode != 308 {
		t.Fatalf("first chunk: %d, want 308", r.StatusCode)
	}
	if r := do(http.MethodPut, session, "bytes 6-10/11", "world"); r.StatusCode != http.StatusOK {
		t.Fatalf("last chunk: %d, want 200", r.StatusCode)
	}

	got := do(http.MethodGet, base+"/storage/v1/b/bkt/o/piped.txt?alt=media", "", "")
	body, _ := io.ReadAll(got.Body)
	if string(body) != "hello world" {
		t.Errorf("object = %q, want %q", body, "hello world")
	}
}
