package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/monirz/cloudrig"
)

func suGet(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func suPost(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestServiceUsageTracksState is the whole point: enable and disable are real,
// so a config that toggles a service round-trips rather than always reading
// "enabled".
func TestServiceUsageTracksState(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/services/run.googleapis.com"

	// Untouched services default to enabled: nothing here gates them.
	if _, got := suGet(t, base); got["state"] != "ENABLED" {
		t.Errorf("default state = %v, want ENABLED", got["state"])
	}

	// Disable is honoured.
	if code := suPost(t, base+":disable"); code != http.StatusOK {
		t.Fatalf("disable = %d", code)
	}
	if _, got := suGet(t, base); got["state"] != "DISABLED" {
		t.Errorf("after disable = %v, want DISABLED", got["state"])
	}

	// Enable brings it back.
	if code := suPost(t, base+":enable"); code != http.StatusOK {
		t.Fatalf("enable = %d", code)
	}
	if _, got := suGet(t, base); got["state"] != "ENABLED" {
		t.Errorf("after enable = %v, want ENABLED", got["state"])
	}
}

// TestServiceUsageListFilter covers state:ENABLED, which `gcloud services list
// --enabled` sends.
func TestServiceUsageListFilter(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	svcs := emu.BaseURL() + "/v1/projects/p/services"

	suPost(t, svcs+"/run.googleapis.com:enable")
	suPost(t, svcs+"/pubsub.googleapis.com:enable")
	suPost(t, svcs+"/storage.googleapis.com:disable")

	_, got := suGet(t, svcs+"?filter=state:ENABLED")
	list, _ := got["services"].([]any)
	names := map[string]bool{}
	for _, s := range list {
		m := s.(map[string]any)
		names[m["name"].(string)] = true
	}
	if !names["projects/p/services/run.googleapis.com"] || !names["projects/p/services/pubsub.googleapis.com"] {
		t.Errorf("enabled list missing services: %v", names)
	}
	if names["projects/p/services/storage.googleapis.com"] {
		t.Errorf("a disabled service appeared in the enabled list")
	}
}

// TestServiceUsageBatchEnable covers the set-enable Terraform uses.
func TestServiceUsageBatchEnable(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/services"

	body := `{"serviceIds":["a.googleapis.com","b.googleapis.com"]}`
	resp, err := http.Post(base+":batchEnable", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batchEnable = %d", resp.StatusCode)
	}
	for _, svc := range []string{"a.googleapis.com", "b.googleapis.com"} {
		if _, got := suGet(t, base+"/"+svc); got["state"] != "ENABLED" {
			t.Errorf("%s = %v after batchEnable", svc, got["state"])
		}
	}
}

// TestServiceUsageDisabledFilter holds the other filter: state:DISABLED must
// return only disabled services, not everything.
func TestServiceUsageDisabledFilter(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	svcs := emu.BaseURL() + "/v1/projects/p/services"

	suPost(t, svcs+"/on.googleapis.com:enable")
	suPost(t, svcs+"/off.googleapis.com:disable")

	_, got := suGet(t, svcs+"?filter=state:DISABLED")
	list, _ := got["services"].([]any)
	names := map[string]bool{}
	for _, s := range list {
		names[s.(map[string]any)["name"].(string)] = true
	}
	if !names["projects/p/services/off.googleapis.com"] {
		t.Error("disabled filter missing the disabled service")
	}
	if names["projects/p/services/on.googleapis.com"] {
		t.Error("disabled filter returned an enabled service")
	}
}

// TestServiceUsageResetClearsState holds that a project reset clears
// enablement state, which lives outside the storage tree.
func TestServiceUsageResetClearsState(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()
	base := emu.BaseURL() + "/v1/projects/p/services/x.googleapis.com"

	if code := suPost(t, base+":disable"); code != http.StatusOK {
		t.Fatal(code)
	}
	if _, got := suGet(t, base); got["state"] != "DISABLED" {
		t.Fatalf("precondition: state = %v", got["state"])
	}

	if err := emu.Reset(ctx, "p"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// After a reset the service is back to the default (enabled), not the
	// disabled value that survived before this fix.
	if _, got := suGet(t, base); got["state"] != "ENABLED" {
		t.Errorf("state after reset = %v, want the default ENABLED", got["state"])
	}
}

// TestServiceUsageConcurrentWrites holds that overlapping enable/disable calls
// leave a consistent state rather than a lost write reported as success.
func TestServiceUsageConcurrentWrites(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/services/race.googleapis.com"

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); suPost(t, base+":enable") }()
		go func() { defer wg.Done(); suPost(t, base+":disable") }()
	}
	wg.Wait()

	// The final state must be whatever the store actually holds — enable or
	// disable, but a real value, and a fresh read must agree with itself.
	_, a := suGet(t, base)
	_, b := suGet(t, base)
	if a["state"] != b["state"] {
		t.Errorf("two reads disagree: %v then %v", a["state"], b["state"])
	}
	if a["state"] != "ENABLED" && a["state"] != "DISABLED" {
		t.Errorf("state = %v, want a real value", a["state"])
	}
}
