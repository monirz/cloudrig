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

	// do sends one request and returns its status, Location and body.
	do := func(method, url, contentRange, body string) (int, string, string) {
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
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Location"), string(b)
	}

	if code, _, _ := do(http.MethodPost, base+"/storage/v1/b?project=p", "", `{"name":"bkt"}`); code != http.StatusOK {
		t.Fatalf("creating the bucket: %d", code)
	}
	code, session, _ := do(http.MethodPost, base+"/resumable/upload/storage/v1/b/bkt/o?uploadType=resumable", "", `{"name":"piped.txt"}`)
	if code != http.StatusOK || session == "" {
		t.Fatalf("opening the session: %d, Location %q", code, session)
	}
	if code, _, _ := do(http.MethodPut, session, "bytes 0-5/*", "hello "); code != http.StatusPermanentRedirect {
		t.Fatalf("first chunk: %d, want 308", code)
	}
	if code, _, _ := do(http.MethodPut, session, "bytes 6-10/11", "world"); code != http.StatusOK {
		t.Fatalf("last chunk: %d, want 200", code)
	}

	if _, _, body := do(http.MethodGet, base+"/storage/v1/b/bkt/o/piped.txt?alt=media", "", ""); body != "hello world" {
		t.Errorf("object = %q, want %q", body, "hello world")
	}
}
