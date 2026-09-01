// Package cloudrun runs Cloud Run services locally.
//
// An image runs as a container, through Docker, because that is what Cloud Run
// deploys. This is the path to use when the container is the thing being
// tested: its base image, its entrypoint, what it bundles.
//
// A source directory instead runs as a process. Cloud Run's contract with your
// code is small — an HTTP server on $PORT — and a process honours it without a
// container build, which is faster and needs no daemon. It is a convenience,
// not an emulation of Cloud Run: nothing about the container is exercised.
//
// The rest of cloudrig needs no Docker. This service does, for images.
package cloudrun

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/logring"
)

// StartupTimeout bounds how long a container may take to listen. Cloud Run's
// own default is four minutes; a local process that has not listened in ten
// seconds has failed, and waiting longer only delays the log that says why.
const StartupTimeout = 10 * time.Second

// Instance is a running service.
type Instance struct {
	name     string
	project  string
	location string
	revision string
	addr     string
	env      []string

	proxy *httputil.ReverseProxy
	logs  *logring.Ring

	// Exactly one of these is set: a service is a container or a process.
	cmd         *exec.Cmd
	containerID string

	stopOnce sync.Once
	cleanup  func()
}

// Options configure a run.
type Options struct {
	// Env is added to the child's environment, as KEY=VALUE.
	Env []string

	// Stderr receives the child's output as well as the log ring.
	Stderr io.Writer
}

// start builds the source, runs it with a PORT of our choosing, and waits for
// it to listen.
func start(ctx context.Context, svc Service, source string, o Options) (*Instance, error) {
	cmd, cleanup, err := build(ctx, svc, source)
	if err != nil {
		return nil, err
	}

	// The port is assigned rather than announced: a Cloud Run service reads
	// $PORT and says nothing, so there is no readiness line to parse. This is
	// the one place the contract differs from a function.
	port, err := freePort()
	if err != nil {
		cleanup()
		return nil, err
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)

	logs := logring.New(logring.DefaultLines)
	out := io.Writer(logs)
	if o.Stderr != nil {
		out = io.MultiWriter(logs, o.Stderr)
	}
	cmd.Stdout, cmd.Stderr = out, out

	// The variables Cloud Run guarantees a container, so code that reads them
	// finds what it expects.
	cmd.Env = append(cmd.Environ(),
		"PORT="+strconv.Itoa(port),
		"K_SERVICE="+svc.Name,
		"K_REVISION="+svc.Revision(),
		"K_CONFIGURATION="+svc.Name,
	)
	cmd.Env = append(cmd.Env, o.Env...)

	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("starting %s: %w", svc.Name, err)
	}
	child := reap(cmd)

	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			child.wait()
		}
		cleanup()
	}
	if err := awaitListen(ctx, addr, child); err != nil {
		tail := logs.Tail(20)
		stop()
		if tail != "" {
			return nil, fmt.Errorf("%s: %w; its output was:\n%s", svc.Name, err, tail)
		}
		return nil, fmt.Errorf("%s: %w", svc.Name, err)
	}

	target, _ := url.Parse("http://" + addr)
	return &Instance{
		name:     svc.Name,
		project:  svc.Project,
		location: svc.Location,
		revision: svc.Revision(),
		addr:     addr,
		env:      o.Env,
		proxy:    httputil.NewSingleHostReverseProxy(target),
		cmd:      cmd,
		logs:     logs,
		cleanup:  stop,
	}, nil
}

// child owns the one call to Wait a process gets.
//
// Two callers need to know when it exits — the startup wait, and Stop — and
// exec.Cmd.Wait may be called once. So it is called here, once, and everyone
// else reads the result.
type child struct {
	done chan struct{}
	err  error
}

func reap(cmd *exec.Cmd) *child {
	c := &child{done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c
}

// wait blocks until the process has exited and returns why.
func (c *child) wait() error {
	<-c.done
	return c.err
}

// retryInterval paces the readiness poll.
const retryInterval = 10 * time.Millisecond

// after returns a channel closed once d has passed on the real clock.
//
// Deliberately the real clock, not the emulator's. Waiting for a child process
// to bind a socket is a wall-clock event: it happens whether or not a test
// advances time, and a FakeClock here would stall every deploy made from a
// test — which is every deploy made from the library.
func after(d time.Duration) <-chan struct{} {
	done := make(chan struct{})
	clock.Real().AfterFunc(d, func() { close(done) })
	return done
}

// awaitListen waits until the service accepts a connection, which is the only
// readiness signal Cloud Run's contract offers.
func awaitListen(ctx context.Context, addr string, c *child) error {
	deadline := after(StartupTimeout)

	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		select {
		// A process that has exited will never listen, so this fails at once
		// rather than waiting out the timeout on a service that crashed.
		case <-c.done:
			return fmt.Errorf("it exited before it listened on $PORT: %w", c.err)
		case <-deadline:
			return fmt.Errorf("it did not listen on $PORT within %s", StartupTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-after(retryInterval):
			// A refused connection comes back at once, so without a pause the
			// loop would spin a core while a service starts.
		}
	}
}

// freePort asks the kernel for a port and releases it. A service is told which
// port to use, so one has to be chosen before it starts.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserving a port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (i *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) { i.proxy.ServeHTTP(w, r) }

func (i *Instance) Name() string          { return i.name }
func (i *Instance) Project() string       { return i.project }
func (i *Instance) Location() string      { return i.location }
func (i *Instance) Revision() string      { return i.revision }
func (i *Instance) URL() string           { return "http://" + i.addr }
func (i *Instance) LogSnapshot() []string { return i.logs.Snapshot() }

// ContainerID is the container behind a service, or empty for one running as a
// process.
func (i *Instance) ContainerID() string { return i.containerID }

// Stop kills the service and removes what building it left behind.
func (i *Instance) Stop() error {
	i.stopOnce.Do(func() {
		if i.cleanup != nil {
			i.cleanup()
		}
	})
	return nil
}
