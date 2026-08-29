package cloudrig_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/core/clock"
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
	// No runner exists yet, so whatever was configured must resolve to none.
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
