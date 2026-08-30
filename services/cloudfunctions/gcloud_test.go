package cloudfunctions_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/functions"
)

// TestGcloudCompatibility drives the API with the real gcloud CLI.
//
// It exists because a compatibility claim decays into nothing without a test
// that actually runs the client: hand-checking once proves the day it was
// checked, and nothing after. Skipped when gcloud is absent so CI stays green
// without it.
func TestGcloudCompatibility(t *testing.T) {
	if testing.Short() {
		t.Skip("runs gcloud against a real emulator")
	}
	if _, err := exec.LookPath("gcloud"); err != nil {
		t.Skip("gcloud is not installed")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t)
	if _, err := emu.Functions().Deploy(context.Background(), functions.Function{
		Project: "my-project", Name: "hello",
		Source: helloDir, EntryPoint: "HelloHTTP",
	}); err != nil {
		t.Fatal(err)
	}

	// The three variables a user sets to point gcloud at the emulator.
	env := append(os.Environ(),
		"CLOUDSDK_CORE_PROJECT=my-project",
		"CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDFUNCTIONS="+emu.BaseURL()+"/",
	)

	run := func(t *testing.T, args ...string) string {
		t.Helper()
		cmd := exec.Command("gcloud", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	t.Run("describe", func(t *testing.T) {
		out := run(t, "functions", "describe", "hello", "--region", "us-central1")
		for _, want := range []string{
			"projects/my-project/locations/us-central1/functions/hello",
			"runtime: go",
			"entryPoint: HelloHTTP",
			"status: ACTIVE",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("describe output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("list reports each function once", func(t *testing.T) {
		out := run(t, "functions", "list")
		// gcloud merges a v1 gen1 list with a v2 GEN_2-filtered list, so a
		// function that answers both filters is listed twice.
		if n := strings.Count(out, "hello"); n != 1 {
			t.Errorf("hello appears %d times, want 1:\n%s", n, out)
		}
		if !strings.Contains(out, "us-central1") {
			t.Errorf("list output missing the region:\n%s", out)
		}
	})

	// The headline: gcloud runs the function, with no generation flag. It
	// works because the v2 descriptor reports GEN_1, so gcloud routes the
	// invocation to v1 :call itself.
	t.Run("call", func(t *testing.T) {
		out := run(t, "functions", "call", "hello",
			"--region", "us-central1", "--data", `{"name":"Monir"}`)
		if !strings.Contains(out, "hello, world") {
			t.Errorf("call output missing the function's response:\n%s", out)
		}
		if !strings.Contains(out, "executionId") {
			t.Errorf("call output missing executionId:\n%s", out)
		}
	})

	t.Run("delete", func(t *testing.T) {
		out := run(t, "functions", "delete", "hello", "--region", "us-central1", "--quiet")
		if !strings.Contains(out, "Deleted") {
			t.Errorf("delete output = %s", out)
		}
		if _, ok := emu.Functions().Get("my-project", "", "hello"); ok {
			t.Error("gcloud reported a delete that did not happen")
		}
	})
}
