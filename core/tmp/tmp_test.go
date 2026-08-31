package tmp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// TestDirIsUnderTheProcessRoot is the whole point: a directory nobody removes
// is still findable, because it is under a root named for this process.
func TestDirIsUnderTheProcessRoot(t *testing.T) {
	dir, err := Dir("thing")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	want := filepath.Join(ParentDir(), strconv.Itoa(os.Getpid()))
	if got := filepath.Dir(dir); got != want {
		t.Errorf("parent = %q, want %q", got, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the directory was not created: %v", err)
	}
}

// TestDirIsUnique covers the two-emulators-one-process case: nothing may be
// handed the same directory twice.
func TestDirIsUnique(t *testing.T) {
	a, err := Dir("thing")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Dir("thing")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(a); _ = os.RemoveAll(b) })

	if a == b {
		t.Errorf("both calls returned %q", a)
	}
}

// TestSweepRemovesDeadRoots is the crash case: a killed emulator ran no
// cleanup, so the next run has to do it.
func TestSweepRemovesDeadRoots(t *testing.T) {
	if _, err := Root(); err != nil {
		t.Fatal(err)
	}

	// A pid that really was a process and really has exited, rather than one
	// picked for being implausible.
	done := exec.Command("sh", "-c", "exit 0")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(ParentDir(), strconv.Itoa(done.Process.Pid))
	if err := os.MkdirAll(filepath.Join(dead, "upload-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dead) })

	if n := Sweep(); n < 1 {
		t.Errorf("Sweep removed %d roots, want at least the dead one", n)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("the dead root survived: %v", err)
	}
}

// TestSweepSpares a live process, which is the expensive mistake: emulators
// run concurrently, and one must never delete another's uploads.
func TestSweepSparesLiveRoots(t *testing.T) {
	mine, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	keep, err := Dir("upload")
	if err != nil {
		t.Fatal(err)
	}

	Sweep()

	if _, err := os.Stat(mine); err != nil {
		t.Errorf("Sweep removed this process's own root: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("Sweep removed a live directory: %v", err)
	}
	_ = os.RemoveAll(keep)
}

// TestAliveOnThisProcess pins the liveness check against the one process we
// know the answer for.
func TestAliveOnThisProcess(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Error("alive() says this process is not running")
	}
	if alive(0) {
		t.Error("alive() says pid 0 is running")
	}
}
