package functions

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"sync"
)

// Instance is a running function: a child process plus the proxy to it.
type Instance struct {
	name     string
	project  string
	location string
	runtime  Runtime
	entry    string
	addr     string
	proxy    *httputil.ReverseProxy
	cmd      *exec.Cmd
	cleanup  func()
	logs     *logRing
	stopped  sync.Once
}

// Options configures Start.
type Options struct {
	// Stderr receives the function's own output. Nil discards it, which is the
	// daemon's default: fn logs is where that output belongs.
	Stderr io.Writer

	// EventLog receives the registry's own messages — a watch redeploying, or
	// failing to. Distinct from Stderr so a daemon can report what it did
	// without echoing every line the function writes.
	EventLog io.Writer

	// Env is extra environment for the child, as KEY=VALUE.
	Env []string
}

// Start builds f and launches it, returning once the child is listening.
//
// Readiness is not polled: the generated shim binds its own port and prints the
// address, so the parent learns it is up by reading one line.
func Start(ctx context.Context, f Function, o Options) (*Instance, error) {
	l, err := f.resolve()
	if err != nil {
		return nil, err
	}

	sp, err := l.prepare(ctx, f)
	if err != nil {
		return nil, err
	}
	cleanup := sp.cleanup

	// One buffer serves both purposes: it is the function's log, and the tail
	// of it explains a child that dies before it ever listens.
	logs := newLogRing(defaultLogLines)
	cmd := sp.cmd
	cmd.Env = append(cmd.Environ(), o.Env...)
	cmd.Stderr = io.Writer(logs)
	if o.Stderr != nil {
		cmd.Stderr = io.MultiWriter(logs, o.Stderr)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("piping stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("starting %s: %w", f.Name, err)
	}

	addr, err := awaitListen(ctx, stdout, logs, o.Stderr, sp.ready)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cleanup()
		if out := strings.TrimSpace(logs.Tail(20)); out != "" {
			return nil, fmt.Errorf("function %s: %w:\n%s", f.Name, err, out)
		}
		return nil, fmt.Errorf("function %s: %w", f.Name, err)
	}

	target, err := url.Parse("http://" + addr)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cleanup()
		return nil, err
	}

	return &Instance{
		name:     f.Name,
		project:  f.Project,
		location: f.Location,
		// From the resolved copy, not the caller's: both may have been
		// detected, and a descriptor built from the request would report empty.
		runtime: f.Runtime,
		entry:   f.EntryPoint,
		addr:    addr,
		proxy:   httputil.NewSingleHostReverseProxy(target),
		cmd:     cmd,
		cleanup: cleanup,
		logs:    logs,
	}, nil
}

// awaitListen waits for the runtime's readiness line, then keeps draining
// stdout so the child never blocks on a full pipe.
func awaitListen(ctx context.Context, stdout io.ReadCloser, logs io.Writer, logTo io.Writer, ready func(string) (string, bool)) (string, error) {
	type result struct {
		addr string
		err  error
	}
	found := make(chan result, 1)

	go func() {
		sc := bufio.NewScanner(stdout)
		announced := false
		for sc.Scan() {
			line := sc.Text()
			if !announced {
				if addr, ok := ready(line); ok {
					found <- result{addr: addr}
					announced = true
					continue
				}
			}
			fmt.Fprintln(logs, line)
			if logTo != nil {
				fmt.Fprintln(logTo, line)
			}
		}
		if !announced {
			found <- result{err: fmt.Errorf("exited before it started listening")}
		}
	}()

	select {
	case r := <-found:
		return r.addr, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Name is the URL segment the function is served under.
func (i *Instance) Name() string { return i.name }

// Runtime is the resolved runtime the function is running on.
func (i *Instance) Runtime() Runtime { return i.runtime }

// EntryPoint is the resolved handler being served.
func (i *Instance) EntryPoint() string { return i.entry }

// LogSnapshot returns the function's recent output.
func (i *Instance) LogSnapshot() []string { return i.logs.Snapshot() }

// FollowLogs streams lines written from now on, and returns a stop function.
func (i *Instance) FollowLogs() (<-chan string, func()) { return i.logs.Follow() }

// Project and Location are the resolved identity of the function.
func (i *Instance) Project() string  { return i.project }
func (i *Instance) Location() string { return i.location }

// URL is the child's own address, bypassing the emulator.
func (i *Instance) URL() string { return "http://" + i.addr }

// ServeHTTP proxies to the child process.
func (i *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) { i.proxy.ServeHTTP(w, r) }

// Stop kills the child and removes its binary.
func (i *Instance) Stop() error {
	var err error
	i.stopped.Do(func() {
		if i.cmd.Process != nil {
			err = i.cmd.Process.Kill()
			_ = i.cmd.Wait()
		}
		i.cleanup()
	})
	return err
}
