package functions_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/functions"
)

// startGo asks for a Go function and returns whatever went wrong. None of
// these reach the compiler, so they cost nothing.
func startGo(t *testing.T, dir, entry string) error {
	t.Helper()
	_, err := functions.Start(context.Background(), functions.Function{
		Name: "fn", Source: dir, Runtime: functions.RuntimeGo, EntryPoint: entry,
	}, functions.Options{})
	if err == nil {
		t.Fatal("a broken source was accepted")
	}
	return err
}

// The source is checked before anything is built, so a wrong path is reported
// as a path rather than as a toolchain failure.
func TestGoSourceMustExist(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "nope")
	err := startGo(t, missing, "Handler")
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want it to name %s", err, missing)
	}
}

// An uploaded tree with no go.mod has nothing to build against, and the
// toolchain's own wording does not say so.
func TestGoSourceMustBeAModule(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "fn.go", `package fn

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) {}
`)

	err := startGo(t, dir, "Handler")
	if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("err = %v, want it to explain the missing module", err)
	}
}

// A directory with its own go.mod is self-contained even when no workspace
// uses it: the fallback outside the workspace is what pointing at it meant.
func TestGoSourceOutsideTheWorkspaceStillResolves(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/standalone\n\ngo 1.25\n")
	write(t, dir, "fn.go", `package standalone

import (
	"net/http"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("standalone"))
}
`)

	inst, err := functions.Start(context.Background(), functions.Function{
		Name: "standalone", Source: dir, Runtime: functions.RuntimeGo, EntryPoint: "Handler",
	}, functions.Options{Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("a standalone module was refused: %v", err)
	}
	t.Cleanup(func() { _ = inst.Stop() })
}

func TestGoEntryPointMustExist(t *testing.T) {
	t.Parallel()

	t.Run("a directory with no handler at all", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		write(t, dir, "go.mod", "module example.com/empty\n\ngo 1.25\n")
		write(t, dir, "fn.go", "package empty\n\nfunc helper() {}\n")

		err := startGo(t, dir, "Handler")
		if !strings.Contains(err.Error(), "no exported func") {
			t.Errorf("err = %v, want it to say there is no handler", err)
		}
	})

	t.Run("a name that is not one of the handlers", func(t *testing.T) {
		t.Parallel()
		err := startGo(t, helloDir, "NotThere")
		if !strings.Contains(err.Error(), "NotThere") {
			t.Errorf("err = %v, want it to name the entry point asked for", err)
		}
	})
}

// A source directory that is a file is not a package, however it is spelled.
func TestGoSourceMustBeADirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "afile.go")
	if err := os.WriteFile(file, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startGo(t, file, "Handler")
}
