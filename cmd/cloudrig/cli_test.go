package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/functions"
)

// runCLI drives run() in-process against a live emulator, capturing the
// combined output. In-process (not an exec'd binary) so it counts as coverage.
func runCLI(t *testing.T, endpoint string, args ...string) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cli")
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == "CLOUDRIG_ENDPOINT" {
			return endpoint, true
		}
		return "", false
	}
	e := run(args, env, f, f)
	_ = f.Close()
	b, _ := os.ReadFile(f.Name())
	return string(b), e
}

func TestCLIFaultLifecycle(t *testing.T) {
	t.Parallel()
	emu := cloudrig.MustStart(t)
	url := emu.BaseURL()

	if out, err := runCLI(t, url, "fault", "storage", "--error", "503"); err != nil {
		t.Fatalf("fault add: %v\n%s", err, out)
	}
	out, err := runCLI(t, url, "fault", "list")
	if err != nil {
		t.Fatalf("fault list: %v", err)
	}
	if !strings.Contains(out, "storage") {
		t.Errorf("fault list = %q, want the storage rule", out)
	}
	if _, err := runCLI(t, url, "fault", "clear"); err != nil {
		t.Fatalf("fault clear: %v", err)
	}
	if out, _ := runCLI(t, url, "fault", "list"); strings.Contains(out, "storage") {
		t.Errorf("rule still armed after clear: %q", out)
	}
}

func TestCLISnapshotRestore(t *testing.T) {
	t.Parallel()
	emu := cloudrig.MustStart(t)
	url := emu.BaseURL()
	path := t.TempDir() + "/state.tar"

	out, err := runCLI(t, url, "snapshot", path)
	if err != nil {
		t.Fatalf("snapshot: %v\n%s", err, out)
	}
	if !strings.Contains(out, "saved snapshot") {
		t.Errorf("snapshot said %q", out)
	}
	out, err = runCLI(t, url, "restore", path)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if !strings.Contains(out, "restored") {
		t.Errorf("restore said %q", out)
	}
}

func TestCLIClock(t *testing.T) {
	t.Parallel()
	emu := cloudrig.MustStart(t, cloudrig.Options{Clock: clock.NewFakeNow()})
	url := emu.BaseURL()

	if out, err := runCLI(t, url, "clock"); err != nil {
		t.Fatalf("clock status: %v\n%s", err, out)
	}
	if out, err := runCLI(t, url, "clock", "advance", "1h"); err != nil {
		t.Fatalf("clock advance: %v\n%s", err, out)
	}
	if out, err := runCLI(t, url, "clock", "goto", "2030-01-01T00:00:00Z"); err != nil {
		t.Fatalf("clock goto: %v\n%s", err, out)
	}
}

func TestCLIFn(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()
	emu := cloudrig.MustStart(t)
	url := emu.BaseURL()

	if _, err := emu.Functions().Deploy(context.Background(), functions.Function{
		Name: "hello", Source: "../../examples/hello", EntryPoint: "HelloHTTP",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, url, "fn", "list")
	if err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("fn list = %q (err %v), want hello", out, err)
	}
	if out, err := runCLI(t, url, "fn", "describe", "hello"); err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("fn describe = %q (err %v)", out, err)
	}
	if out, err := runCLI(t, url, "fn", "invoke", "hello", "--data", `{"name":"Monir"}`); err != nil {
		t.Fatalf("fn invoke: %v\n%s", err, out)
	}
	if _, err := runCLI(t, url, "fn", "delete", "hello"); err != nil {
		t.Fatalf("fn delete: %v", err)
	}
}

// TestCLIErrors covers the "no emulator" and bad-argument paths.
func TestCLIErrors(t *testing.T) {
	t.Parallel()
	// A port nothing listens on: the client reports a friendly error.
	if _, err := runCLI(t, "http://127.0.0.1:1", "fault", "list"); err == nil {
		t.Error("fault list against a dead endpoint should error")
	}
	if _, err := runCLI(t, "http://127.0.0.1:1", "snapshot"); err == nil {
		t.Error("snapshot with no file argument should error")
	}
}
