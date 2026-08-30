package functions_test

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/functions"
)

const nodeDir = "../testdata/node-hello"

func TestDetectRuntime(t *testing.T) {
	t.Parallel()

	t.Run("go from source files", func(t *testing.T) {
		t.Parallel()
		got, err := functions.DetectRuntime(helloDir)
		if err != nil || got != functions.RuntimeGo {
			t.Errorf("DetectRuntime = %q, %v; want go", got, err)
		}
	})

	t.Run("node from package.json", func(t *testing.T) {
		t.Parallel()
		got, err := functions.DetectRuntime(nodeDir)
		if err != nil || got != functions.RuntimeNode20 {
			t.Errorf("DetectRuntime = %q, %v; want nodejs20", got, err)
		}
	})

	t.Run("neither", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(dir+"/README", []byte("hi"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := functions.DetectRuntime(dir); err == nil {
			t.Error("guessed a runtime for a directory with neither")
		}
	})
}

func TestKnownRuntimes(t *testing.T) {
	t.Parallel()

	got := strings.Join(functions.KnownRuntimes(), ",")
	for _, want := range []string{"go", "nodejs20", "nodejs22"} {
		if !strings.Contains(got, want) {
			t.Errorf("KnownRuntimes = %s, missing %s", got, want)
		}
	}
}

// TestNodeFunction runs a real @google-cloud/functions-framework child. It is
// the proof that the launcher abstraction is not Go-shaped: Node is told its
// port and announces readiness on stdout, where Go picks its own and prints it.
func TestNodeFunction(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a node subprocess")
	}
	if !exists(nodeDir + "/node_modules/.bin/functions-framework") {
		t.Skipf("run: (cd %s && npm i)", nodeDir)
	}
	t.Parallel()

	inst, err := functions.Start(context.Background(), functions.Function{
		Name: "hello", Source: nodeDir, Runtime: functions.RuntimeNode20, EntryPoint: "handler",
	}, functions.Options{Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop() })

	if inst.Runtime() != functions.RuntimeNode20 {
		t.Errorf("Runtime() = %q, want nodejs20", inst.Runtime())
	}

	srv := httptest.NewServer(inst)
	t.Cleanup(srv.Close)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"query parameter", "/?name=Monir", "Hello, Monir!"},
		{"default", "/", "Hello, cloudrig!"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.TrimSpace(get(t, srv.URL+tc.path)); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNodeNeedsAnEntryPoint(t *testing.T) {
	t.Parallel()

	// Only Go can be inspected without running anything, so Node must be told.
	_, err := functions.Start(context.Background(), functions.Function{
		Name: "hello", Source: nodeDir, Runtime: functions.RuntimeNode20,
	}, functions.Options{})
	if err == nil || !strings.Contains(err.Error(), "needs an explicit entry point") {
		t.Errorf("err = %v, want a request for an entry point", err)
	}
}

func TestNodeReportsAMissingFramework(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"x"}`)
	_, err := functions.Start(context.Background(), functions.Function{
		Name: "x", Source: dir, EntryPoint: "handler",
	}, functions.Options{})
	if err == nil || !strings.Contains(err.Error(), "npm i @google-cloud/functions-framework") {
		t.Errorf("err = %v, want the npm install hint", err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
