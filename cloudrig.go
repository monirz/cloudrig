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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/events"
	"github.com/monirz/cloudrig/core/tmp"
	"github.com/monirz/cloudrig/functions"
	"google.golang.org/grpc"

	"github.com/monirz/cloudrig/services/cloudfunctions"
	"github.com/monirz/cloudrig/services/pubsub"
	"github.com/monirz/cloudrig/services/storage"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
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

	// Runner is "auto", "subprocess" or "none". "auto" resolves to
	// "subprocess" when Functions is non-empty, else "none".
	Runner string

	// Functions are built and launched at Start, and stopped at Shutdown.
	Functions []functions.Function

	// FunctionLog receives function output. Nil discards it; use fn logs.
	FunctionLog io.Writer

	// EventLog receives the emulator's own messages, such as a watched
	// function redeploying. Nil discards them.
	EventLog io.Writer

	// DataDir persists Cloud Storage across restarts: metadata snapshots and
	// object content live under it. Empty keeps everything in memory and a
	// temp directory, which is what a test wants — state surviving a test is a
	// bug, not a feature.
	DataDir string
}

// Emulator is a running instance.
type Emulator struct {
	clk      clock.Clock
	addr     string
	shutdown func(context.Context) error
	fns      *functions.Registry
	storage  *storage.Service
	bus      *events.Bus
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

	// Whatever a previous run left behind when it was killed. Only the roots
	// of processes that are gone: a running emulator's are left alone.
	tmp.SweepOnce()

	// One bus for the whole emulator: it is the only path between services.
	bus := events.New()

	reg, err := newRegistry(ctx, clk, bus, o)
	if err != nil {
		return nil, err
	}
	stack, err := newStorage(clk, bus, o.DataDir)
	if err != nil {
		reg.StopAll()
		return nil, err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		reg.StopAll()
		stack.close()
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	psvc := pubsub.New(stack.kvStore, clk, bus)
	handler, closeAPIs := newHandler(clk, o, reg, stack.svc, psvc, newGRPC(psvc))
	srv := &http.Server{
		Handler:   handler,
		Protocols: transport.Protocols(), // HTTP/1.1 and h2c on one port
	}
	go func() {
		_ = srv.Serve(ln) // ErrServerClosed is what Shutdown returns
	}()

	return &Emulator{
		clk:     clk,
		addr:    dialable(ln.Addr().String()),
		fns:     reg,
		storage: stack.svc,
		bus:     bus,
		shutdown: func(ctx context.Context) error {
			err := srv.Shutdown(ctx)
			reg.StopAll()
			stack.close()
			closeAPIs()
			return err
		},
	}, nil
}

// storageStack is the Cloud Storage service and what it must tear down.
type storageStack struct {
	svc     *storage.Service
	blobs   *blob.Store
	kv      io.Closer // non-nil only when persisting
	kvStore store.Store
}

func (s storageStack) close() {
	if s.kv != nil {
		_ = s.kv.Close()
	}
	if s.blobs != nil {
		_ = s.blobs.Close()
	}
}

// newStorage builds the Cloud Storage service and the blob tree it streams
// payloads into. A DataDir makes both survive a restart.
func newStorage(clk clock.Clock, bus *events.Bus, dataDir string) (storageStack, error) {
	if dataDir == "" {
		blobs, err := blob.NewTemp()
		if err != nil {
			return storageStack{}, fmt.Errorf("cloudrig: %w", err)
		}
		kv := store.NewMemory()
		return storageStack{svc: storage.New(kv, blobs, clk, bus), blobs: blobs, kvStore: kv}, nil
	}

	blobs, err := blob.New(filepath.Join(dataDir, "storage"))
	if err != nil {
		return storageStack{}, fmt.Errorf("cloudrig: %w", err)
	}
	kv, err := store.OpenPersistent(filepath.Join(dataDir, "storage", "metadata.json"), clk)
	if err != nil {
		_ = blobs.Close()
		return storageStack{}, fmt.Errorf("cloudrig: %w", err)
	}
	return storageStack{svc: storage.New(kv, blobs, clk, bus), blobs: blobs, kv: kv, kvStore: kv}, nil
}

// newRegistry deploys everything configured up front, tearing down whatever
// came up if a later one fails.
func newRegistry(ctx context.Context, clk clock.Clock, bus *events.Bus, o Options) (*functions.Registry, error) {
	reg := functions.NewRegistry(clk, bus, functions.Options{
		Stderr:   o.FunctionLog,
		EventLog: o.EventLog,
	})
	for _, f := range o.Functions {
		if _, err := reg.Deploy(ctx, f); err != nil {
			reg.StopAll()
			return nil, err
		}
	}
	return reg, nil
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

	tmp.SweepOnce()
	bus := events.New()

	reg, err := newRegistry(context.Background(), o.Clock, bus, o)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(reg.StopAll)

	// Never persisted: state surviving a test is a bug, not a feature.
	stack, err := newStorage(o.Clock, bus, "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(stack.close)

	psvc := pubsub.New(stack.kvStore, o.Clock, bus)
	handler, closeAPIs := newHandler(o.Clock, o, reg, stack.svc, psvc, newGRPC(psvc))
	t.Cleanup(closeAPIs)

	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = transport.Protocols()
	srv.Start()
	t.Cleanup(srv.Close)

	return &Emulator{
		clk:     o.Clock,
		addr:    srv.Listener.Addr().String(),
		fns:     reg,
		storage: stack.svc,
		bus:     bus,
		shutdown: func(context.Context) error {
			srv.Close()
			reg.StopAll()
			stack.close()
			closeAPIs()
			return nil
		},
	}
}

// newGRPC registers every gRPC service on a server.
//
// Requests reach it through the transport's h2c dispatch, so gRPC and REST
// share the one port.
func newGRPC(ps *pubsub.Service) *grpc.Server {
	srv := grpc.NewServer()
	pubsubpb.RegisterPublisherServer(srv, pubsub.NewPublisher(ps))
	pubsubpb.RegisterSubscriberServer(srv, pubsub.NewSubscriber(ps))
	return srv
}

// routeV1 sends a request to Pub/Sub when it has a route for it, and to Cloud
// Functions otherwise. Both APIs are /v1/projects/{project}/...
func routeV1(ps *pubsub.REST, fns http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ps.Matches(r.Method, r.URL.EscapedPath()) {
			ps.ServeHTTP(w, r)
			return
		}
		fns.ServeHTTP(w, r)
	})
}

// newHandler builds the request surface and returns what it must tear down:
// the API objects own temporary directories, and nothing else can reach them.
func newHandler(clk clock.Clock, o Options, reg *functions.Registry, gcs *storage.Service, psvc *pubsub.Service, grpcSrv http.Handler) (http.Handler, func()) {
	configured := o.Runner
	if configured == "" {
		configured = "auto"
	}
	// The runner is in force whenever a registry can accept a deploy, not only
	// once something is deployed: functions can arrive at any time.
	mode := "none"
	if configured != "none" {
		mode = "subprocess"
	}

	// The v1 API is a view over the same registry the runner uses, never a
	// second store: an API that keeps its own copy is how an emulator ends up
	// reporting a deploy that runs nothing.
	mounts := map[string]http.Handler{}

	api := cloudfunctions.New(reg, clk, o.EventLog)
	for _, prefix := range cloudfunctions.Prefixes {
		mounts[prefix] = api
	}
	closers := []io.Closer{api}

	// Pub/Sub's JSON API lives under the same /v1/projects/{project}/ prefix
	// as Cloud Functions, so a mount prefix cannot tell them apart: the one
	// with a route for the request takes it.
	mounts["/v1/"] = routeV1(pubsub.NewREST(psvc), api)

	var gcsAPI http.Handler
	if gcs != nil {
		storageAPI := storage.NewAPI(gcs)
		gcsAPI = storageAPI
		closers = append(closers, storageAPI)
		for _, prefix := range storage.Prefixes {
			mounts[prefix] = storageAPI
		}
	}

	h := transport.New(transport.Config{
		Clock:    clk,
		Version:  o.Version,
		Fallback: gcsAPI,
		// Mode is what is in force, Configured what was asked for.
		Runner:    transport.RunnerInfo{Configured: configured, Mode: mode},
		Functions: reg,
		Mounts:    mounts,
		GRPC:      grpcSrv,
		Reset: func(ctx context.Context, project string) error {
			if gcs == nil {
				return nil
			}
			return gcs.Reset(ctx, project)
		},
	})

	return h, func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}
}

// Reset clears emulator state. An empty project clears everything.
func (e *Emulator) Reset(ctx context.Context, project string) error {
	if e.storage == nil {
		return nil
	}
	return e.storage.Reset(ctx, project)
}

// SyncEvents waits for every event published so far to reach its subscribers.
//
// Delivery is asynchronous so a write never waits on a trigger. A test that
// uploads an object and then asserts a function ran needs this, or it would
// have to poll.
func (e *Emulator) SyncEvents() {
	if e.bus != nil {
		e.bus.Sync()
	}
}

// Functions is the registry backing this emulator, so a test can deploy into a
// running instance rather than only at Start.
func (e *Emulator) Functions() *functions.Registry { return e.fns }

// FunctionURL is where the named function is served, or "" if it is not
// deployed. It returns the short form, valid for the default project and
// location; the prefixed form is also served.
func (e *Emulator) FunctionURL(name string) string {
	if _, ok := e.fns.Get("", "", name); !ok {
		return ""
	}
	return e.BaseURL() + "/" + name
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
