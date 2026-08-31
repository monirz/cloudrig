// Package tmp puts every temporary directory the emulator makes under one
// process-owned root.
//
// Scattered os.MkdirTemp calls leak: a directory is removed by its owner's
// Close, and a killed process runs no Close. One root per process means a
// crash leaves exactly one thing behind, and the next run can tell it from a
// live emulator's and remove it.
package tmp

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// parent holds one directory per emulator process.
const parent = "cloudrig"

var (
	once     sync.Once
	root     string
	rootErr  error
	sweepOne sync.Once
)

// ParentDir is where every process root lives.
func ParentDir() string { return filepath.Join(os.TempDir(), parent) }

// Root returns this process's directory, creating it on first use.
func Root() (string, error) {
	once.Do(func() {
		if rootErr = os.MkdirAll(ParentDir(), 0o700); rootErr != nil {
			return
		}
		root = filepath.Join(ParentDir(), strconv.Itoa(os.Getpid()))
		rootErr = os.MkdirAll(root, 0o700)
	})
	return root, rootErr
}

// Dir makes a uniquely named directory under this process's root.
func Dir(name string) (string, error) {
	r, err := Root()
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(r, name+"-")
}

// RemoveRoot deletes this process's root and everything under it.
func RemoveRoot() error {
	r, err := Root()
	if err != nil {
		return err
	}
	return os.RemoveAll(r)
}

// SweepOnce removes leftover roots the first time it is called in a process.
func SweepOnce() int {
	var removed int
	sweepOne.Do(func() { removed = Sweep() })
	return removed
}

// Sweep removes the roots of processes that are no longer running, and reports
// how many it removed. Errors are ignored: another emulator may be sweeping
// the same directory, and losing that race is not a failure.
func Sweep() int {
	entries, err := os.ReadDir(ParentDir())
	if err != nil {
		return 0
	}

	var removed int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() || alive(pid) {
			continue
		}
		if os.RemoveAll(filepath.Join(ParentDir(), e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// alive reports whether a process is still running. Anything that cannot be
// answered counts as alive: leaving a directory behind costs a few kilobytes,
// deleting a live emulator's uploads costs it the upload.
func alive(pid int) bool {
	// Not real pids: signalling 0 reaches this process's whole group, and a
	// negative one reaches a group too. Neither can own a root.
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	switch err := p.Signal(syscall.Signal(0)); {
	case err == nil:
		return true
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return false
	}
	return true
}
