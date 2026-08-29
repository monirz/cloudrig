// Package cloudrig is a local emulator for Google Cloud APIs.
//
// Start runs it as a server; MustStart runs it in-process inside a Go test:
//
//	func TestUpload(t *testing.T) {
//		t.Parallel()
//		emu := cloudrig.MustStart(t)
//		// ... point a GCP client at emu.BaseURL()
//	}
//
// MustStart is the reason the project exists: no container, no daemon, and one
// isolated instance per test.
package cloudrig

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/transport"
)

// DefaultAddr binds every interface so the binary is reachable from a
// container. MustStart ignores it and takes a random free loopback port.
const DefaultAddr = ":4599"

// Options configures an Emulator. The zero value is valid.
type Options struct {
	// Addr is the listen address. Empty means DefaultAddr for Start, a random
	// free loopback port for MustStart.
	Addr string

	// Clock is the only source of time. Empty means real for Start, fake for
	// MustStart: a test that cannot control time will eventually sleep.
	Clock clock.Clock

	// Version is reported by /_emu/health. Empty means "dev".
	Version string

	// Runner is "auto", "subprocess" or "none". Every value resolves to "none"
	// until a runner exists, and health reports both.
	Runner string
}

// Emulator is a running instance.
type Emulator struct {
	clk      clock.Clock
	addr     string
	shutdown func(context.Context) error
}

// Start runs the emulator on a real listener; the caller owns shutdown. ctx
// bounds startup only and does not stop the server.
func Start(ctx context.Context, o Options) (*Emulator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	addr := o.Addr
	if addr == "" {
		addr = DefaultAddr
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real()
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cloudrig: listen on %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:   newHandler(clk, o),
		Protocols: transport.Protocols(), // HTTP/1.1 and h2c on one port
	}
	go func() {
		_ = srv.Serve(ln) // ErrServerClosed is what Shutdown returns
	}()

	return &Emulator{
		clk:      clk,
		addr:     dialable(ln.Addr().String()),
		shutdown: srv.Shutdown,
	}, nil
}

// MustStart runs the emulator in-process for one test, on a random free port
// with a FakeClock and t.Cleanup registered. Every call is fully isolated.
func MustStart(t testing.TB, opts ...Options) *Emulator {
	t.Helper()

	var o Options
	switch len(opts) {
	case 0:
	case 1:
		o = opts[0]
	default:
		t.Fatalf("cloudrig: MustStart takes at most one Options, got %d", len(opts))
	}

	if o.Clock == nil {
		o.Clock = clock.NewFake(fakeEpoch())
	}
	if o.Addr != "" {
		t.Fatalf("cloudrig: MustStart binds its own port; Options.Addr must be empty, got %q", o.Addr)
	}

	srv := httptest.NewUnstartedServer(newHandler(o.Clock, o))
	srv.Config.Protocols = transport.Protocols()
	srv.Start()
	t.Cleanup(srv.Close)

	return &Emulator{
		clk:  o.Clock,
		addr: srv.Listener.Addr().String(),
		shutdown: func(context.Context) error {
			srv.Close()
			return nil
		},
	}
}

func newHandler(clk clock.Clock, o Options) http.Handler {
	configured := o.Runner
	if configured == "" {
		configured = "auto"
	}
	return transport.New(transport.Config{
		Clock:   clk,
		Version: o.Version,
		// Mode is reported separately from Configured so health stays honest
		// while no runner exists.
		Runner: transport.RunnerInfo{Configured: configured, Mode: "none"},
	})
}

// Endpoint is the host:port a client should dial, e.g. "127.0.0.1:53412".
func (e *Emulator) Endpoint() string { return e.addr }

// dialable turns a listen address into a connectable one: binding ":4599"
// reports "[::]:4599", which is a wildcard for accepting, not for dialing.
func dialable(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return net.JoinHostPort(host, port)
}

// BaseURL is the endpoint as a URL: what STORAGE_EMULATOR_HOST and the SDK
// endpoint overrides want.
func (e *Emulator) BaseURL() string { return "http://" + e.addr }

// Clock returns the emulator's clock.
func (e *Emulator) Clock() clock.Clock { return e.clk }

// FakeClock returns the clock as a *clock.FakeClock so a test can Advance it,
// failing t if the emulator is running a real one.
func (e *Emulator) FakeClock(t testing.TB) *clock.FakeClock {
	t.Helper()
	fake, ok := e.clk.(*clock.FakeClock)
	if !ok {
		t.Fatalf("cloudrig: emulator is running a %T, not a *clock.FakeClock", e.clk)
	}
	return fake
}

// Shutdown stops the emulator; MustStart registers it with t.Cleanup.
func (e *Emulator) Shutdown(ctx context.Context) error { return e.shutdown(ctx) }
