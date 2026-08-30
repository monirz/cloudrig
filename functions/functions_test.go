package functions_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/functions"
)

const helloDir = "../examples/hello"

func TestEntryPoints(t *testing.T) {
	t.Parallel()

	got, err := functions.EntryPoints(helloDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Echo,HelloHTTP"; strings.Join(got, ",") != want {
		t.Errorf("EntryPoints = %v, want %s", got, want)
	}
}

func TestDetectEntryPoint(t *testing.T) {
	t.Parallel()

	t.Run("refuses to guess between several", func(t *testing.T) {
		t.Parallel()
		_, err := functions.DetectEntryPoint(helloDir)
		if err == nil || !strings.Contains(err.Error(), "pass --entry-point") {
			t.Errorf("err = %v, want a request for --entry-point", err)
		}
	})

	t.Run("picks the only one", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		write(t, dir, "fn.go", `package solo

import "net/http"

func Only(w http.ResponseWriter, r *http.Request) {}

// NotAHandler has the wrong signature and must not be a candidate.
func NotAHandler(w http.ResponseWriter) {}

// unexported is not a candidate either.
func unexported(w http.ResponseWriter, r *http.Request) {}
`)
		got, err := functions.DetectEntryPoint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != "Only" {
			t.Errorf("DetectEntryPoint = %q, want Only", got)
		}
	})

	t.Run("reports a package with none", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		write(t, dir, "fn.go", "package empty\n")
		if _, err := functions.DetectEntryPoint(dir); err == nil {
			t.Error("accepted a package with no handler")
		}
	})
}

func TestStartValidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   functions.Function
		want string
	}{
		{"no name", functions.Function{Source: helloDir, EntryPoint: "HelloHTTP"}, "name is empty"},
		{"slash in name", functions.Function{Name: "a/b", Source: helloDir, EntryPoint: "HelloHTTP"}, "path character"},
		{"no source", functions.Function{Name: "h", EntryPoint: "HelloHTTP"}, "no source directory"},
		{"ambiguous entry point is refused", functions.Function{Name: "h", Source: helloDir}, "pass --entry-point"},
		{"lowercase entry point is not a Go handler", functions.Function{Name: "h", Source: helloDir, EntryPoint: "hello"}, `no entry point "hello"`},
		{"unknown runtime", functions.Function{Name: "h", Source: helloDir, Runtime: "ruby33", EntryPoint: "HelloHTTP"}, `unknown runtime "ruby33"`},
		{"missing entry point", functions.Function{Name: "h", Source: helloDir, EntryPoint: "Nope"}, `no entry point "Nope"`},
		{"missing source", functions.Function{Name: "h", Source: "./does-not-exist", EntryPoint: "X"}, "no such file"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inst, err := functions.Start(context.Background(), tc.fn, functions.Options{})
			if inst != nil {
				_ = inst.Stop()
			}
			if err == nil {
				t.Fatalf("accepted %+v", tc.fn)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestStartAndServe(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	inst, err := functions.Start(context.Background(), functions.Function{
		Name: "hello", Source: helloDir, EntryPoint: "HelloHTTP",
	}, functions.Options{Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop() })

	if inst.Name() != "hello" {
		t.Errorf("Name() = %q", inst.Name())
	}
	if !strings.HasPrefix(inst.URL(), "http://127.0.0.1:") {
		t.Errorf("URL() = %q, want a loopback address", inst.URL())
	}

	srv := httptest.NewServer(inst)
	t.Cleanup(srv.Close)

	t.Run("query reaches the function", func(t *testing.T) {
		body := get(t, srv.URL+"/?name=monir")
		if !strings.Contains(body, `"greeting":"hello, monir"`) {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("a request body streams through", func(t *testing.T) {
		// Echo is a second handler in the same package, so this also proves the
		// shim serves the entry point it was asked for and not just the first.
		echo, err := functions.Start(context.Background(), functions.Function{
			Name: "echo", Source: helloDir, EntryPoint: "Echo",
		}, functions.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = echo.Stop() })

		es := httptest.NewServer(echo)
		t.Cleanup(es.Close)

		resp, err := http.Post(es.URL+"/", "text/plain", strings.NewReader("streamed"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if string(b) != "streamed" {
			t.Errorf("echo returned %q, want %q", b, "streamed")
		}
	})
}

func TestStopIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	inst, err := functions.Start(context.Background(), functions.Function{
		Name: "hello", Source: helloDir, EntryPoint: "HelloHTTP",
	}, functions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := inst.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestBuildFailureIsReported(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	// A handler whose body does not compile: the entry point check passes, so
	// the failure has to survive the build step with its output attached.
	dir := t.TempDir()
	write(t, dir, "go.mod", "module broken\n\ngo 1.25\n")
	write(t, dir, "fn.go", `package broken

import "net/http"

func Broken(w http.ResponseWriter, r *http.Request) {
	undefinedSymbol()
}
`)
	_, err := functions.Start(context.Background(), functions.Function{
		Name: "broken", Source: dir, EntryPoint: "Broken",
	}, functions.Options{})
	if err == nil {
		t.Fatal("a package that does not compile started successfully")
	}
	if !strings.Contains(err.Error(), "undefinedSymbol") {
		t.Errorf("err = %q, want the compiler output", err)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestSelfContainedModuleInsideAWorkspace guards a bug where a function with
// its own go.mod failed under a go.work that did not `use` it: the toolchain
// refuses the package outright, so the launcher falls back to GOWORK=off.
func TestSelfContainedModuleInsideAWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	// A workspace listing only itself, with the function module nested under
	// it and deliberately absent from `use`.
	root := t.TempDir()
	write(t, root, "go.work", "go 1.25\n\nuse .\n")
	write(t, root, "go.mod", "module example.com/outer\n\ngo 1.25\n")
	write(t, root, "outer.go", "package outer\n")

	fnDir := root + "/fn"
	if err := os.MkdirAll(fnDir, 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, fnDir, "go.mod", "module example.com/inner\n\ngo 1.25\n")
	write(t, fnDir, "fn.go", `package inner

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) { w.Write([]byte("inner ok")) }
`)

	inst, err := functions.Start(context.Background(), functions.Function{
		Name: "inner", Source: fnDir,
	}, functions.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop() })

	srv := httptest.NewServer(inst)
	t.Cleanup(srv.Close)
	if got := get(t, srv.URL+"/"); got != "inner ok" {
		t.Errorf("body = %q, want %q", got, "inner ok")
	}
}
