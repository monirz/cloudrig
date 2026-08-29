package main_test

import (
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// buildBinary compiles cmd/cloudrig for the tests below.
func buildBinary(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "cloudrig")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/cloudrig: %v\n%s", err, out)
	}
	return bin
}

// freePort asks the kernel for a port and releases it. Racy, but better than
// hardcoding 4599 and colliding with a running emulator.
func freePort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting %s: %v", ln.Addr(), err)
	}
	return port
}

// waitForHealth polls until the child serves or the deadline passes.
func waitForHealth(t *testing.T, url string, within time.Duration) *http.Response {
	t.Helper()

	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			return resp
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never came up within %s: %v", url, within, lastErr)
	return nil
}

// TestStartServesHealth is acceptance criterion 1, against the real binary.
func TestStartServesHealth(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	t.Parallel()

	bin := buildBinary(t)
	port := freePort(t)

	cmd := exec.Command(bin, "start", "--port", port)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	resp := waitForHealth(t, "http://127.0.0.1:"+port+"/_emu/health", 20*time.Second)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
}

// TestGracefulShutdown covers SIGTERM: a binary that only dies when killed
// severs in-flight requests and hangs CI.
func TestGracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	t.Parallel()

	bin := buildBinary(t)
	port := freePort(t)

	cmd := exec.Command(bin, "start", "--port", port)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	resp := waitForHealth(t, "http://127.0.0.1:"+port+"/_emu/health", 20*time.Second)
	resp.Body.Close()

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("waiting for exit: %v", err)
		}
		if exit != nil {
			t.Errorf("exited %v after SIGTERM, want a clean exit", exit)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("did not exit within 15s of SIGTERM")
	}
}

// TestEnvMirrorsFlags proves the container path: -e CLOUDRIG_PORT, no flags.
func TestEnvMirrorsFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	t.Parallel()

	bin := buildBinary(t)
	port := freePort(t)

	cmd := exec.Command(bin, "start")
	cmd.Env = append(os.Environ(), "CLOUDRIG_PORT="+port)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	resp := waitForHealth(t, "http://127.0.0.1:"+port+"/_emu/health", 20*time.Second)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	t.Parallel()

	bin := buildBinary(t)
	out, err := exec.Command(bin, "serve").CombinedOutput()
	if err == nil {
		t.Fatalf("exited 0 on an unknown command; output:\n%s", out)
	}
}
