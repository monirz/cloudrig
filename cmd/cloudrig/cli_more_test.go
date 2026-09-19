package main

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/tmp"
)

// serveUntilSignalled runs a serving command in-process, waits for its health
// check, then signals this process the way Ctrl-C would. Not parallel: the
// signal reaches every listener in the binary, and parallel tests only start
// once the sequential ones have finished.
func serveUntilSignalled(t *testing.T, port int, args ...string) string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "serve")
	if err != nil {
		t.Fatal(err)
	}
	noEnv := func(string) (string, bool) { return "", false }
	done := make(chan error, 1)
	go func() { done <- run(args, noEnv, f, f) }()

	health := "http://127.0.0.1:" + strconv.Itoa(port) + "/_emu/health"
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(health)
		if err == nil {
			resp.Body.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("%v exited before serving: %v", args, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v never served: %v", args, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("%v did not stop on SIGTERM", args)
	}
	_ = f.Close()
	out, _ := os.ReadFile(f.Name())
	return string(out)
}

func openPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestStartInProcess(t *testing.T) {
	port := openPort(t)
	out := serveUntilSignalled(t, port, "start", "--port", strconv.Itoa(port),
		"--clock", "virtual", "--clock-start", "2030-01-01T00:00:00Z", "--data-dir", t.TempDir())

	// start removes the process's temp root on exit, which the other tests
	// in this binary share; put it back.
	if root, err := tmp.Root(); err != nil || os.MkdirAll(root, 0o700) != nil {
		t.Fatalf("restoring the temp root: %v", err)
	}

	for _, want := range []string{"listening on", "persisting storage under", "shutting down"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFnRunInProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	port := openPort(t)
	out := serveUntilSignalled(t, port, "fn", "run", "../../examples/hello",
		"--entry-point", "HelloHTTP", "--port", strconv.Itoa(port))

	if !strings.Contains(out, "function: ") {
		t.Errorf("output does not name the function URL:\n%s", out)
	}
}

func TestCLIFnDeployAndLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()
	url := cloudrig.MustStart(t).BaseURL()

	out, err := runCLI(t, url, "fn", "deploy", "hello", "--source", "../../examples/hello",
		"--entry-point", "HelloHTTP", "--trigger-topic", "greetings")
	if err != nil {
		t.Fatalf("fn deploy: %v\n%s", err, out)
	}
	for _, want := range []string{"deployed ", "url: ", "trigger: "} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy output lacks %q:\n%s", want, out)
		}
	}

	if out, err := runCLI(t, url, "fn", "logs", "hello", "--project", "cloudrig-local"); err != nil {
		t.Fatalf("fn logs: %v\n%s", err, out)
	}
	if _, err := runCLI(t, url, "fn", "logs", "missing"); err == nil {
		t.Error("fn logs of a missing function should error")
	}
	if _, err := runCLI(t, url, "fn", "describe", "missing"); err == nil {
		t.Error("fn describe of a missing function should error")
	}
	if _, err := runCLI(t, url, "fn", "delete", "hello"); err != nil {
		t.Fatalf("fn delete: %v", err)
	}
	if out, _ := runCLI(t, url, "fn", "list"); !strings.Contains(out, "no functions deployed") {
		t.Errorf("fn list after delete = %q", out)
	}
}

// TestCLIUsageErrors covers argument checking, which fails before any request.
func TestCLIUsageErrors(t *testing.T) {
	t.Parallel()
	const dead = "http://127.0.0.1:1"

	for _, args := range [][]string{
		{"fn"},
		{"fn", "bogus"},
		{"fn", "deploy"},
		{"fn", "deploy", "hello"},
		{"fn", "deploy", "--bogus"},
		{"fn", "deploy", "hello", "--source", "."},
		{"fn", "invoke"},
		{"fn", "invoke", "hello", "extra"},
		{"fn", "invoke", "hello", "--bogus"},
		{"fn", "logs"},
		{"fn", "logs", "hello", "extra"},
		{"fn", "logs", "hello", "--bogus"},
		{"fn", "logs", "hello"},
		{"fn", "describe"},
		{"fn", "describe", "hello", "extra"},
		{"fn", "delete"},
		{"fn", "delete", "hello", "--bogus"},
		{"fn", "list", "extra"},
		{"fn", "run"},
		{"clock", "bogus"},
		{"clock", "status", "extra"},
		{"clock", "status", "--bogus"},
		{"clock", "freeze", "extra"},
		{"clock", "freeze"},
		{"clock", "advance"},
		{"clock", "advance", "--bogus"},
		{"clock", "goto"},
		{"clock", "goto", "--bogus"},
		{"fault"},
		{"fault", "bogus"},
		{"fault", "storage", "--bogus"},
		{"fault", "storage", "--failure-rate", "lots"},
		{"fault", "storage", "--failure-rate", "-5"},
		{"fault", "list", "--bogus"},
		{"fault", "clear", "--bogus"},
		{"fault", "clear"},
		{"snapshot", "a", "b"},
		{"snapshot", "--bogus"},
		{"snapshot", "-"},
		{"snapshot", t.TempDir() + "/missing/state.tar"},
		{"restore", t.TempDir() + "/missing.tar"},
	} {
		if out, err := runCLI(t, dead, args...); err == nil {
			t.Errorf("%v succeeded:\n%s", args, out)
		}
	}

	for _, args := range [][]string{{"clock", "help"}, {"fault", "--help"}} {
		if out, err := runCLI(t, dead, args...); err == nil || !strings.Contains(out, "usage:") {
			t.Errorf("%v = %v, want help:\n%s", args, err, out)
		}
	}
}

func TestCLIFaultShapes(t *testing.T) {
	t.Parallel()
	url := cloudrig.MustStart(t).BaseURL()

	out, err := runCLI(t, url, "fault", "secrets", "--timeout", "--latency", "1s",
		"--failure-rate", "20%", "--count", "2", "--message", "slow")
	if err != nil {
		t.Fatalf("fault add: %v\n%s", err, out)
	}
	if want := "timeout, latency 1s, 20% of requests, first 2"; !strings.Contains(out, want) {
		t.Errorf("armed = %q, want %q", out, want)
	}
	if out, err := runCLI(t, url, "fault", "any", "--path", "/custom/", "--failure-rate", "0.5"); err != nil {
		t.Fatalf("fault --path: %v\n%s", err, out)
	}

	out, err = runCLI(t, url, "fault", "list")
	if err != nil {
		t.Fatal(err)
	}
	// secretmanager and secrets share a prefix; the list names the first.
	if !strings.Contains(out, "secretmanager →") || !strings.Contains(out, "/custom/ →") {
		t.Errorf("fault list = %q", out)
	}
	if out, _ := runCLI(t, url, "fault", "clear"); !strings.Contains(out, "cleared") {
		t.Errorf("fault clear = %q", out)
	}
	if out, _ := runCLI(t, url, "fault", "list"); !strings.Contains(out, "no faults armed") {
		t.Errorf("fault list after clear = %q", out)
	}
}

func TestCLIClockFreeze(t *testing.T) {
	t.Parallel()

	virtual := cloudrig.MustStart(t, cloudrig.Options{Clock: clock.NewFakeNow()}).BaseURL()
	if out, err := runCLI(t, virtual, "clock", "freeze"); err != nil || !strings.Contains(out, "clock frozen") {
		t.Errorf("freeze on a virtual clock = %q (err %v)", out, err)
	}
	if out, err := runCLI(t, virtual, "clock", "show"); err != nil || !strings.Contains(out, "virtual") {
		t.Errorf("clock show = %q (err %v)", out, err)
	}
	if _, err := runCLI(t, virtual, "clock", "advance", "soon"); err == nil {
		t.Error("advancing by a malformed duration should error")
	}

	real := cloudrig.MustStart(t, cloudrig.Options{Clock: clock.Real()}).BaseURL()
	if _, err := runCLI(t, real, "clock", "freeze"); err == nil {
		t.Error("freeze on a real clock should error")
	}
}

func TestCLISnapshotToStdout(t *testing.T) {
	t.Parallel()
	url := cloudrig.MustStart(t).BaseURL()

	out, err := runCLI(t, url, "snapshot", "-")
	if err != nil {
		t.Fatalf("snapshot -: %v", err)
	}
	if len(out) == 0 {
		t.Error("snapshot - wrote nothing")
	}

	junk := t.TempDir() + "/junk.tar"
	if err := os.WriteFile(junk, []byte("not a tar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, url, "restore", junk); err == nil {
		t.Error("restoring a malformed archive should error")
	}
}

func TestEnvelopeMessage(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]string{
		`{"error":{"message":"nope"}}`: "nope",
		"plain failure\n":              "plain failure",
		"":                             "500 Internal Server Error",
	} {
		resp := &http.Response{Status: "500 Internal Server Error", Body: readCloser(body)}
		if got := envelopeMessage(resp); got != want {
			t.Errorf("envelopeMessage(%q) = %q, want %q", body, got, want)
		}
	}
}

func readCloser(s string) *nopBody { return &nopBody{strings.NewReader(s)} }

type nopBody struct{ *strings.Reader }

func (nopBody) Close() error { return nil }
