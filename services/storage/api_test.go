package storage_test

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/services/storage"
)

// step is one request against the JSON API and the status it must get.
type step struct {
	method, path, body string
	header             map[string]string
	want               int
}

// TestAPIWalk drives the HTTP handlers in order over one bucket, covering the
// happy paths and each malformed-parameter and missing-resource branch.
func TestAPIWalk(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	api := storage.NewAPI(svc)
	t.Cleanup(func() { _ = api.Close() })
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	const (
		b  = "/storage/v1/b"
		o  = b + "/bkt/o"
		up = "/upload/storage/v1/b/bkt/o"
	)
	multipart := func(boundary string) map[string]string {
		return map[string]string{"Content-Type": "multipart/related; boundary=" + boundary}
	}
	const goodPart = "--xyz\r\nContent-Type: application/json\r\n\r\n{\"name\":\"m.txt\"}\r\n" +
		"--xyz\r\nContent-Type: text/plain\r\n\r\nmultipart body\r\n--xyz--\r\n"

	steps := []step{
		// Buckets.
		{method: "POST", path: b, body: `{"name":"bkt"}`, want: 400},
		{method: "POST", path: b + "?project=p", body: `{`, want: 400},
		{method: "POST", path: b + "?project=p", body: `{"name":"bkt","versioning":{"enabled":true}}`, want: 200},
		{method: "POST", path: b + "?project=p", body: `{"name":"bkt"}`, want: 409},
		{method: "GET", path: b, want: 400},
		{method: "GET", path: b + "?project=p", want: 200},
		{method: "GET", path: b + "/missing", want: 404},
		{method: "GET", path: b + "/bkt/storageLayout", want: 200},
		{method: "GET", path: b + "/missing/storageLayout", want: 404},
		{method: "PATCH", path: b + "/bkt", body: `{`, want: 400},
		{method: "PATCH", path: b + "/bkt?ifMetagenerationMatch=x", body: `{}`, want: 400},
		{method: "PATCH", path: b + "/bkt?ifMetagenerationMatch=99", body: `{"storageClass":"NEARLINE"}`, want: 412},
		{method: "PATCH", path: b + "/bkt", body: `{"versioning":{"enabled":false}}`, want: 200},
		{method: "PATCH", path: b + "/missing", body: `{}`, want: 404},

		// Media and multipart uploads.
		{method: "POST", path: up + "?uploadType=media&name=a.txt", body: "hello", want: 200},
		{method: "POST", path: up + "?uploadType=bogus&name=x", want: 400},
		{method: "POST", path: up + "?ifGenerationMatch=x&name=x", want: 400},
		{method: "POST", path: "/upload/storage/v1/b/missing/o?name=x", want: 404},
		{
			method: "POST", path: up + "?uploadType=multipart",
			header: map[string]string{"Content-Type": "multipart/related"}, want: 400,
		},
		{method: "POST", path: up + "?uploadType=multipart", header: multipart("''"), want: 400},
		{method: "POST", path: up + "?uploadType=multipart", header: multipart("xyz"), want: 400},
		{
			method: "POST", path: up + "?uploadType=multipart", header: multipart("xyz"),
			body: "--xyz\r\n\r\n{\r\n--xyz--\r\n", want: 400,
		},
		{
			method: "POST", path: up + "?uploadType=multipart", header: multipart("xyz"),
			body: "--xyz\r\n\r\n{}\r\n--xyz--\r\n", want: 400,
		},
		{
			method: "POST", path: up + "?uploadType=multipart", header: multipart("xyz"),
			body: goodPart, want: 200,
		},

		// Reads.
		{method: "GET", path: o + "/a.txt", want: 200},
		{method: "GET", path: o + "/a.txt?generation=x", want: 400},
		{method: "GET", path: o + "/a.txt?ifGenerationMatch=x", want: 400},
		{method: "GET", path: o + "/a.txt?alt=media", want: 200},
		{method: "GET", path: o + "/a.txt?alt=media&ifGenerationMatch=1", want: 412},
		{method: "GET", path: o + "/nope?alt=media", want: 404},
		{method: "GET", path: b + "/missing/o/a.txt", want: 404},
		{method: "GET", path: "/bkt/a.txt", want: 200},
		{method: "HEAD", path: "/bkt/a.txt", want: 200},
		{method: "GET", path: "/bkt/a.txt?generation=x", want: 400},
		{method: "GET", path: "/bkt/a.txt?ifGenerationNotMatch=x", want: 400},
		{method: "GET", path: "/bkt/nope", want: 404},
		{method: "GET", path: "/missing/a.txt", want: 404},
		{method: "GET", path: o + "?maxResults=x", want: 400},
		{method: "GET", path: o + "?maxResults=1", want: 200},
		{method: "GET", path: o + "?delimiter=/", want: 200},
		{method: "GET", path: b + "/missing/o", want: 404},

		// Metadata updates.
		{method: "PATCH", path: o + "/a.txt", body: `{`, want: 400},
		{method: "PATCH", path: o + "/a.txt?ifMetagenerationNotMatch=x", body: `{}`, want: 400},
		{method: "PATCH", path: o + "/a.txt", body: `{"contentType":"text/plain","metadata":{"k":"v"}}`, want: 200},
		{method: "PATCH", path: o + "/nope", body: `{}`, want: 404},
		{method: "PATCH", path: b + "/missing/o/a.txt", body: `{}`, want: 404},

		// Copy, rewrite and compose.
		{method: "POST", path: o + "/a.txt/copyTo/b/bkt/o/c.txt", body: `{}`, want: 200},
		{method: "POST", path: o + "/a.txt/rewriteTo/b/bkt/o/r.txt", body: `{}`, want: 200},
		{method: "POST", path: o + "/a.txt/copyTo/b/bkt/o/c.txt?ifSourceGenerationMatch=1", body: `{}`, want: 501},
		{method: "POST", path: o + "/a.txt/copyTo/b/bkt/o/c.txt?ifGenerationMatch=x", body: `{}`, want: 400},
		{method: "POST", path: o + "/a.txt/copyTo/b/bkt/o/c.txt?sourceGeneration=x", body: `{}`, want: 400},
		{method: "POST", path: o + "/a.txt/copyTo/b/bkt/o/c.txt", body: `{`, want: 400},
		{method: "POST", path: o + "/a.txt/copyTo/b/missing/o/c.txt", body: `{}`, want: 404},
		{method: "POST", path: o + "/a.txt/rewriteTo/b/missing/o/c.txt", body: `{}`, want: 404},
		{method: "POST", path: b + "/missing/o/a.txt/copyTo/b/bkt/o/c.txt", body: `{}`, want: 404},
		{
			method: "POST", path: o + "/d.txt/compose",
			body: `{"sourceObjects":[{"name":"a.txt"},{"name":"c.txt"}]}`, want: 200,
		},
		{
			method: "POST", path: o + "/d.txt/compose",
			body: `{"sourceObjects":[{"name":"a.txt","generation":999999}]}`, want: 404,
		},
		{method: "POST", path: o + "/d.txt/compose", body: `{`, want: 400},
		{method: "POST", path: o + "/d.txt/compose?ifSourceGenerationMatch=1", body: `{}`, want: 501},
		{method: "POST", path: b + "/missing/o/d.txt/compose", body: `{}`, want: 404},

		// IAM.
		{method: "GET", path: b + "/bkt/iam", want: 200},
		{
			method: "PUT", path: b + "/bkt/iam",
			body: `{"bindings":[{"role":"roles/storage.objectViewer","members":["allUsers"]}]}`, want: 200,
		},
		{method: "GET", path: b + "/bkt/iam", want: 200},
		{method: "PUT", path: b + "/bkt/iam", body: `{`, want: 400},
		{method: "GET", path: o + "/a.txt/iam", want: 200},
		{method: "PUT", path: o + "/a.txt/iam", body: `{}`, want: 200},
		{method: "GET", path: b + "/missing/iam", want: 404},
		{method: "PUT", path: b + "/missing/iam", body: `{}`, want: 404},
		{method: "GET", path: b + "/bkt/iam/testPermissions?permissions=storage.objects.get", want: 200},
		{method: "GET", path: b + "/missing/iam/testPermissions", want: 404},

		// The XML surface a signed URL points at.
		{method: "PUT", path: "/bkt/e.txt", body: "signed", want: 200},
		{method: "PUT", path: "/bkt/e.txt?ifGenerationMatch=x", want: 400},
		{method: "PUT", path: "/missing/e.txt", want: 404},
		{method: "DELETE", path: "/bkt/e.txt?ifGenerationMatch=x", want: 400},
		{method: "DELETE", path: "/bkt/e.txt", want: 204},
		{method: "DELETE", path: "/missing/e.txt", want: 404},

		// Deletes.
		{method: "DELETE", path: o + "/a.txt?ifGenerationMatch=x", want: 400},
		{method: "DELETE", path: b + "/missing/o/a.txt", want: 404},
		{method: "DELETE", path: o + "/a.txt", want: 204},
		{method: "DELETE", path: b + "/bkt", want: 409},
		{method: "DELETE", path: b + "/missing", want: 404},
	}

	for _, s := range steps {
		if got, body := do(t, srv.URL, s); got != s.want {
			t.Errorf("%s %s = %d, want %d: %s", s.method, s.path, got, s.want, body)
		}
	}
}

// TestAPIResumable walks a resumable session through each chunk outcome.
func TestAPIResumable(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	api := storage.NewAPI(svc)
	t.Cleanup(func() { _ = api.Close() })
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	const up = "/upload/storage/v1/b/bkt/o?uploadType=resumable"
	for _, s := range []step{
		{method: "POST", path: "/storage/v1/b?project=p", body: `{"name":"bkt"}`, want: 200},
		{method: "POST", path: up, body: `{}`, want: 400},
		{method: "POST", path: up, body: `{`, want: 400},
		{method: "POST", path: "/upload/storage/v1/b/missing/o?uploadType=resumable&name=x", want: 404},
		{method: "PUT", path: up + "&upload_id=unknown", want: 404},
	} {
		if got, body := do(t, srv.URL, s); got != s.want {
			t.Fatalf("%s %s = %d, want %d: %s", s.method, s.path, got, s.want, body)
		}
	}

	session := func() string {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+up, strings.NewReader(`{"name":"r.txt"}`))
		req.Header.Set("X-Upload-Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode != 200 || loc == "" {
			t.Fatalf("starting a session = %d, Location %q", resp.StatusCode, loc)
		}
		return strings.TrimPrefix(loc, srv.URL)
	}
	chunk := func(path, contentRange, body string, extra map[string]string) step {
		h := map[string]string{"Content-Range": contentRange}
		maps.Copy(h, extra)
		return step{method: "PUT", path: path, body: body, header: h}
	}

	loc := session()
	for _, s := range []step{
		withWant(chunk(loc, "garbage", "", nil), 400),
		withWant(chunk(loc, "bytes 0-2/*", "abc", nil), 308),
		withWant(chunk(loc, "bytes */*", "", nil), 308),
		withWant(chunk(loc, "bytes 7-8/*", "xy", nil), 308),
		withWant(chunk(loc, "bytes */*", "", map[string]string{"X-GUploader-No-308": "yes"}), 200),
		withWant(chunk(loc, "bytes 3-4/5", "de", nil), 200),
		withWant(chunk(loc, "bytes */5", "", nil), 404),
	} {
		if got, body := do(t, srv.URL, s); got != s.want {
			t.Errorf("PUT %s = %d, want %d: %s", s.header["Content-Range"], got, s.want, body)
		}
	}

	// A chunk shorter than its declared range is refused.
	short := session()
	if got, body := do(t, srv.URL, withWant(chunk(short, "bytes 0-9/*", "abc", nil), 400)); got != 400 {
		t.Errorf("short chunk = %d: %s", got, body)
	}

	// A query that finds every byte already present finalises the upload.
	whole := session()
	do(t, srv.URL, chunk(whole, "bytes 0-1/*", "ok", nil))
	if got, body := do(t, srv.URL, chunk(whole, "bytes */2", "", nil)); got != 200 {
		t.Errorf("finalising query = %d: %s", got, body)
	}
}

func withWant(s step, want int) step { s.want = want; return s }

func do(t *testing.T, base string, s step) (int, string) {
	t.Helper()
	req, err := http.NewRequest(s.method, base+s.path, strings.NewReader(s.body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range s.header {
		req.Header.Set(k, v)
	}
	// A 308 is an answer here, not a redirect to follow.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
