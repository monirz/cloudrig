package transport_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/transport"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// serve starts a listener speaking both HTTP/1.1 and cleartext HTTP/2, exactly
// as cmd/cloudrig and the library entry point will.
func serve(t *testing.T, cfg transport.Config) *httptest.Server {
	t.Helper()
	if cfg.Clock == nil {
		cfg.Clock = clock.NewFake(epoch)
	}
	srv := httptest.NewUnstartedServer(transport.New(cfg))
	srv.Config.Protocols = transport.Protocols()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// h2cClient speaks cleartext HTTP/2 with prior knowledge, the way a gRPC client
// does. Without this the gRPC dispatch branch could only be reached with a
// synthetic request, which is not evidence that a real client reaches it.
func h2cClient() *http.Client {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: tr}
}

func TestOnePortServesBothProtocols(t *testing.T) {
	t.Parallel()
	srv := serve(t, transport.Config{})

	t.Run("HTTP/1.1 reaches the REST mux", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/_emu/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.ProtoMajor != 1 {
			t.Errorf("proto = %s, want HTTP/1.x", resp.Proto)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("cleartext HTTP/2 is served on the same port", func(t *testing.T) {
		resp, err := h2cClient().Get(srv.URL + "/_emu/health")
		if err != nil {
			t.Fatalf("h2c request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Errorf("proto = %s, want HTTP/2.0", resp.Proto)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
}

func TestGRPCDispatch(t *testing.T) {
	t.Parallel()
	srv := serve(t, transport.Config{})

	tests := []struct {
		name        string
		contentType string
		wantStatus  int
		wantReason  string
	}{
		{
			name:        "application/grpc over h2c gets 501",
			contentType: "application/grpc",
			wantStatus:  http.StatusNotImplemented,
			wantReason:  "notImplemented",
		},
		{
			// Real gRPC clients send a subtype, so the discrimination is a
			// prefix test rather than equality.
			name:        "application/grpc+proto is still gRPC",
			contentType: "application/grpc+proto",
			wantStatus:  http.StatusNotImplemented,
			wantReason:  "notImplemented",
		},
		{
			// HTTP/2 alone is not gRPC: an ordinary h2 client hitting an
			// unknown REST path must get the REST 404, not the gRPC 501.
			name:        "HTTP/2 without the gRPC content type is REST",
			contentType: "application/json",
			wantStatus:  http.StatusNotFound,
			wantReason:  "notFound",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/pkg.Service/Method", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", tc.contentType)

			resp, err := h2cClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.ProtoMajor != 2 {
				t.Fatalf("request did not go over HTTP/2: %s", resp.Proto)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := reason(t, resp); got != tc.wantReason {
				t.Errorf("reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}

func TestGRPC501NamesTheOperation(t *testing.T) {
	t.Parallel()
	srv := serve(t, transport.Config{})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/google.pubsub.v1.Publisher/Publish", http.NoBody)
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	// Rule 5: a 501 that does not say what was not implemented is not loud.
	for _, want := range []string{"google.pubsub.v1.Publisher/Publish", "no gRPC services are registered"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body %s does not mention %q", body, want)
		}
	}
}

// TestPercentEncodedSegment is acceptance criterion 4. An object named
// logs/2026/app.log arrives as one percent-encoded segment; routing on
// r.URL.Path would decode it first and split it into three.
func TestPercentEncodedSegment(t *testing.T) {
	t.Parallel()

	var got string
	h := transport.New(transport.Config{Clock: clock.NewFake(epoch)})
	transport.ExportRoute(h, http.MethodGet, "/probe/{a}/o/{b}",
		func(w http.ResponseWriter, r *http.Request, p transport.Params) error {
			got = p["b"]
			w.WriteHeader(http.StatusNoContent)
			return nil
		})

	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = transport.Protocols()
	srv.Start()
	defer srv.Close()

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "encoded slashes stay in one segment", path: "/probe/bkt/o/logs%2F2026%2Fapp.log", want: "logs/2026/app.log"},
		{name: "a plain name is unaffected", path: "/probe/bkt/o/app.log", want: "app.log"},
		{name: "encoded percent", path: "/probe/bkt/o/a%25b", want: "a%b"},
		{name: "encoded hash, legal in a GCS object name", path: "/probe/bkt/o/a%23b", want: "a#b"},
		{name: "spaces and unicode", path: "/probe/bkt/o/caf%C3%A9%20x", want: "café x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got = ""
			// Build the URL with the escaped path preserved: parsing the string
			// would be enough here, but Opaque-free construction keeps net/http
			// from re-encoding it.
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
			if got != tc.want {
				t.Errorf("captured %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRouting(t *testing.T) {
	t.Parallel()
	srv := serve(t, transport.Config{})

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantReason string
	}{
		{name: "known route", method: http.MethodGet, path: "/_emu/health", wantStatus: 200},
		{name: "unknown path is 404", method: http.MethodGet, path: "/nope", wantStatus: 404, wantReason: "notFound"},
		{name: "wrong method is 405, not 404", method: http.MethodPost, path: "/_emu/health", wantStatus: 405, wantReason: "methodNotAllowed"},
		{name: "a prefix of a route is not the route", method: http.MethodGet, path: "/_emu", wantStatus: 404, wantReason: "notFound"},
		{name: "an extra segment is not the route", method: http.MethodGet, path: "/_emu/health/x", wantStatus: 404, wantReason: "notFound"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, srv.URL+tc.path, http.NoBody)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantReason != "" {
				if got := reason(t, resp); got != tc.wantReason {
					t.Errorf("reason = %q, want %q", got, tc.wantReason)
				}
			}
		})
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	fake := clock.NewFake(epoch)
	srv := serve(t, transport.Config{
		Clock:   fake,
		Version: "1.2.3",
		Runner:  transport.RunnerInfo{Configured: "auto", Mode: "none"},
	})

	// Uptime is read from the injected clock, so advancing the fake clock must
	// move it. That makes health an end-to-end check that the clock really is
	// injected rather than shadowed by a time.Now somewhere below.
	fake.Advance(90 * time.Second)

	resp, err := http.Get(srv.URL + "/_emu/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got transport.Health
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := transport.Health{
		Status:  "ok",
		Version: "1.2.3",
		Uptime:  "1m30s",
		Runner:  transport.RunnerInfo{Configured: "auto", Mode: "none"},
	}
	if got != want {
		t.Errorf("health = %+v, want %+v", got, want)
	}
}

func TestHealthDefaults(t *testing.T) {
	t.Parallel()
	srv := serve(t, transport.Config{})

	resp, err := http.Get(srv.URL + "/_emu/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got transport.Health
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "dev" {
		t.Errorf("version = %q, want %q", got.Version, "dev")
	}
	// No runner exists yet, so health must not claim one does.
	if got.Runner.Mode != "none" {
		t.Errorf("runner.mode = %q, want %q", got.Runner.Mode, "none")
	}
}

func reason(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	if len(body.Error.Errors) == 0 {
		return ""
	}
	return body.Error.Errors[0].Reason
}
