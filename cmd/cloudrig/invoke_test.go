package main_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/functions"
)

// TestFnInvoke drives the real binary against a live emulator, because the
// value of a CLI subcommand is entirely in what a shell sees.
func TestFnInvoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and compiles a function")
	}
	t.Parallel()

	bin := buildBinary(t)
	emu := cloudrig.MustStart(t)

	deploy := func(t *testing.T, f functions.Function) {
		t.Helper()
		if _, err := emu.Functions().Deploy(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	deploy(t, functions.Function{Name: "hello", Source: "../../examples/hello", EntryPoint: "HelloHTTP"})

	// A function that fails, to prove the error path exits non-zero.
	broken := t.TempDir()
	write(t, broken, "go.mod", "module example.com/boom\n\ngo 1.25\n")
	write(t, broken, "fn.go", `package boom

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "it broke", http.StatusInternalServerError)
}
`)
	deploy(t, functions.Function{Name: "boom", Source: broken})

	run := func(t *testing.T, stdin string, args ...string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "CLOUDRIG_ENDPOINT="+emu.BaseURL())
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}

	t.Run("data reaches the function", func(t *testing.T) {
		out, _, err := run(t, "", "fn", "invoke", "hello", "--data", `{"name":"Monir"}`)
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		if !strings.Contains(out, `"greeting":"hello, world"`) {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("no data is allowed", func(t *testing.T) {
		if _, _, err := run(t, "", "fn", "invoke", "hello"); err != nil {
			t.Errorf("invoke with no --data: %v", err)
		}
	})

	t.Run("stdin with --data -", func(t *testing.T) {
		out, _, err := run(t, `{"name":"piped"}`, "fn", "invoke", "hello", "--data", "-")
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		if !strings.Contains(out, "hello, world") {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("a failing function exits non-zero with its message", func(t *testing.T) {
		_, errOut, err := run(t, "", "fn", "invoke", "boom")
		if err == nil {
			t.Fatal("a function that returned 500 exited 0")
		}
		if !strings.Contains(errOut, "it broke") {
			t.Errorf("stderr = %q, want the function's own message", errOut)
		}
	})

	t.Run("a missing function names the resource", func(t *testing.T) {
		_, errOut, err := run(t, "", "fn", "invoke", "nope")
		if err == nil {
			t.Fatal("invoking a missing function exited 0")
		}
		if !strings.Contains(errOut, "functions/nope does not exist") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("no name is rejected", func(t *testing.T) {
		_, errOut, err := run(t, "", "fn", "invoke")
		if err == nil || !strings.Contains(errOut, "needs a function name") {
			t.Errorf("err = %v, stderr = %q", err, errOut)
		}
	})
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
