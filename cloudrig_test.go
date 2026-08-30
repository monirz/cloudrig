package cloudrig_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/functions"
)

func health(t *testing.T, emu *cloudrig.Emulator) map[string]any {
	t.Helper()
	resp, err := http.Get(emu.BaseURL() + "/_emu/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding health: %v", err)
	}
	return body
}

func TestMustStartServesHealth(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	if got := health(t, emu)["status"]; got != "ok" {
		t.Errorf("status = %v, want ok", got)
	}
	if emu.Endpoint() == "" {
		t.Error("Endpoint() is empty")
	}
	if want := "http://" + emu.Endpoint(); emu.BaseURL() != want {
		t.Errorf("BaseURL() = %q, want %q", emu.BaseURL(), want)
	}
}

// TestParallelInstancesAreIsolated is acceptance criterion 2: distinct ports
// and no shared state. It is the property the project exists for.
func TestParallelInstancesAreIsolated(t *testing.T) {
	t.Parallel()

	ports := make(chan string, 2)
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			emu := cloudrig.MustStart(t)

			// A shared clock would be a shared-state bug a port check misses.
			emu.FakeClock(t).Advance(time.Duration(len(name)) * time.Hour)
			if got := health(t, emu)["status"]; got != "ok" {
				t.Fatalf("status = %v", got)
			}
			ports <- emu.Endpoint()
		})
	}

	t.Cleanup(func() {
		close(ports)
		seen := map[string]bool{}
		for p := range ports {
			if seen[p] {
				t.Errorf("two instances shared the endpoint %s", p)
			}
			seen[p] = true
		}
		if len(seen) != 2 {
			t.Errorf("saw %d distinct endpoints, want 2", len(seen))
		}
	})
}

func TestMustStartDefaultsToAFakeClock(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	fake := emu.FakeClock(t)

	// Advancing here must show up in the served health body.
	fake.Advance(2 * time.Hour)
	if got := health(t, emu)["uptime"]; got != "2h0m0s" {
		t.Errorf("uptime = %v, want 2h0m0s", got)
	}
}

func TestMustStartAcceptsOptions(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t, cloudrig.Options{Version: "9.9.9", Runner: "subprocess"})
	body := health(t, emu)

	if got := body["version"]; got != "9.9.9" {
		t.Errorf("version = %v, want 9.9.9", got)
	}
	runner := body["runner"].(map[string]any)
	if runner["configured"] != "subprocess" {
		t.Errorf("runner.configured = %v, want subprocess", runner["configured"])
	}
	// The runner is in force because the registry can accept a deploy, not
	// only once something is deployed.
	if runner["mode"] != "subprocess" {
		t.Errorf("runner.mode = %v, want subprocess", runner["mode"])
	}
}

func TestRunnerCanBeDisabled(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t, cloudrig.Options{Runner: "none"})
	runner := health(t, emu)["runner"].(map[string]any)
	if runner["mode"] != "none" {
		t.Errorf("runner.mode = %v, want none", runner["mode"])
	}
}

func TestStartOnARealListener(t *testing.T) {
	t.Parallel()

	// Port 0 avoids colliding with a developer already on 4599.
	emu, err := cloudrig.Start(context.Background(), cloudrig.Options{
		Addr:    "127.0.0.1:0",
		Version: "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = emu.Shutdown(context.Background()) })

	if got := health(t, emu)["status"]; got != "ok" {
		t.Errorf("status = %v, want ok", got)
	}

	// Start defaults to a real clock, so the fake accessor must refuse.
	if _, ok := emu.Clock().(*clock.FakeClock); ok {
		t.Error("Start defaulted to a FakeClock; want a real clock")
	}
}

func TestStartRejectsABadAddress(t *testing.T) {
	t.Parallel()

	if _, err := cloudrig.Start(context.Background(), cloudrig.Options{Addr: "not-an-address"}); err == nil {
		t.Error("Start accepted a malformed address")
	}
}

func TestShutdownStopsServing(t *testing.T) {
	t.Parallel()

	emu, err := cloudrig.Start(context.Background(), cloudrig.Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	url := emu.BaseURL() + "/_emu/health"
	if _, err := http.Get(url); err != nil {
		t.Fatalf("health before shutdown: %v", err)
	}

	if err := emu.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Error("still serving after Shutdown")
	}
}

func TestCleanupClosesTheInstance(t *testing.T) {
	t.Parallel()

	var url string
	t.Run("inner", func(t *testing.T) {
		emu := cloudrig.MustStart(t)
		url = emu.BaseURL() + "/_emu/health"
		if _, err := http.Get(url); err != nil {
			t.Fatalf("health inside the test: %v", err)
		}
	})

	// Its t.Cleanup has run; a leak here would be one listener per test.
	if _, err := http.Get(url); err == nil {
		t.Error("instance still serving after its test finished")
	}
}

func TestBaseURLIsDialable(t *testing.T) {
	t.Parallel()

	// A wildcard bind reports "[::]:port", which is for accepting, not dialing.
	emu, err := cloudrig.Start(context.Background(), cloudrig.Options{Addr: ":0"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = emu.Shutdown(context.Background()) })

	if strings.Contains(emu.Endpoint(), "::") || strings.HasPrefix(emu.Endpoint(), "0.0.0.0") {
		t.Errorf("Endpoint() = %q, want a dialable host", emu.Endpoint())
	}
	// The real proof is that it connects.
	if got := health(t, emu)["status"]; got != "ok" {
		t.Errorf("status = %v via %s", got, emu.BaseURL())
	}
}

// TestFunctionInsideATest is what the project is for: a Cloud Function built,
// launched and served inside a Go test, with no Docker and no daemon.
func TestFunctionInsideATest(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t, cloudrig.Options{
		Functions: []functions.Function{{
			Name: "hello", Source: "./examples/hello", EntryPoint: "HelloHTTP",
		}},
	})

	if got, want := emu.FunctionURL("hello"), emu.BaseURL()+"/hello"; got != want {
		t.Errorf("FunctionURL = %q, want %q", got, want)
	}
	if emu.FunctionURL("absent") != "" {
		t.Errorf("FunctionURL for an unknown name = %q, want empty", emu.FunctionURL("absent"))
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"query reaches the function", "/hello?name=monir", `"greeting":"hello, monir"`},
		{"root of the mount", "/hello", `"path":"/"`},
		// A function is a subtree, so it is the root of its own URL space.
		{"subtree path is rewritten", "/hello/a/b", `"path":"/a/b"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(emu.BaseURL() + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body = %s, want it to contain %s", body, tc.want)
			}
		})
	}

	t.Run("health reports the runner in force", func(t *testing.T) {
		runner := health(t, emu)["runner"].(map[string]any)
		if runner["mode"] != "subprocess" {
			t.Errorf("runner.mode = %v, want subprocess", runner["mode"])
		}
	})

	t.Run("emulator routes are unaffected", func(t *testing.T) {
		if got := health(t, emu)["status"]; got != "ok" {
			t.Errorf("status = %v", got)
		}
	})
}

func TestFunctionsAreIsolatedPerInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	// Two parallel tests, each with its own function process. A shared runner
	// would show up as a shared port.
	urls := make(chan string, 2)
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			emu := cloudrig.MustStart(t, cloudrig.Options{
				Functions: []functions.Function{{
					Name: "hello", Source: "./examples/hello", EntryPoint: "HelloHTTP",
				}},
			})
			if _, err := http.Get(emu.FunctionURL("hello")); err != nil {
				t.Fatal(err)
			}
			urls <- emu.BaseURL()
		})
	}

	t.Cleanup(func() {
		close(urls)
		seen := map[string]bool{}
		for u := range urls {
			if seen[u] {
				t.Errorf("two instances shared %s", u)
			}
			seen[u] = true
		}
	})
}

func TestStartReportsAFunctionFailure(t *testing.T) {
	t.Parallel()

	_, err := cloudrig.Start(context.Background(), cloudrig.Options{
		Addr: "127.0.0.1:0",
		Functions: []functions.Function{{
			Name: "bad", Source: "./examples/hello", EntryPoint: "NotThere",
		}},
	})
	if err == nil {
		t.Fatal("Start succeeded with a missing entry point")
	}
	if !strings.Contains(err.Error(), "NotThere") {
		t.Errorf("err = %q, want it to name the entry point", err)
	}
}

// TestDeployIntoARunningEmulator is the shape the CLI uses: start first, deploy
// afterwards, over the admin API.
func TestDeployIntoARunningEmulator(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t)
	if emu.FunctionURL("hello") != "" {
		t.Fatal("a fresh emulator reported a function")
	}

	body := `{"name":"hello","source":"./examples/hello","entryPoint":"HelloHTTP"}`
	resp, err := http.Post(emu.BaseURL()+functions.AdminPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("deploy status = %d: %s", resp.StatusCode, out)
	}

	if got := emu.FunctionURL("hello"); got != emu.BaseURL()+"/hello" {
		t.Errorf("FunctionURL = %q", got)
	}
	out := get(t, emu.BaseURL()+"/hello?name=deployed")
	if !strings.Contains(out, "hello, deployed") {
		t.Errorf("body = %s", out)
	}
}

func TestFunctionsRegistryIsReachable(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	// The library gets the registry directly, so a test can deploy without
	// going through HTTP.
	emu := cloudrig.MustStart(t)
	if _, err := emu.Functions().Deploy(context.Background(), functions.Function{
		Name: "hello", Source: "./examples/hello", EntryPoint: "HelloHTTP",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(get(t, emu.BaseURL()+"/hello?name=lib"), "hello, lib") {
		t.Error("function deployed through the registry is not served")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, body)
	}
	return string(body)
}
