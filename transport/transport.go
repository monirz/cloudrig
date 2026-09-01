// Package transport is cloudrig's front door: one handler on one port, serving
// REST over HTTP/1.1 and gRPC over cleartext HTTP/2 at once.
package transport

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/faults"
	"github.com/monirz/cloudrig/core/gerr"
)

// Config wires a Handler. Clock is required; everything else has a default.
type Config struct {
	Clock clock.Clock

	// Version is reported by /_emu/health. Empty means "dev".
	Version string

	// Runner describes the function runner.
	Runner RunnerInfo

	// Functions hosts deployed functions. Nil means none are served.
	Functions FunctionHost

	// Mounts are service handlers keyed by path prefix, tried before the
	// built-in mux. Services own their own routing, so the front door needs no
	// knowledge of their URL grammar.
	Mounts map[string]http.Handler

	// Fallback handles paths no route claimed. Cloud Storage needs it: the Go
	// client downloads from /{bucket}/{object}, which has no prefix to mount
	// on and is only distinguishable from anything else by asking.
	Fallback http.Handler

	// Reset clears emulator state. An empty project clears everything.
	Reset func(ctx context.Context, project string) error

	// GRPC handles requests on the gRPC branch. Nil answers them with 501.
	GRPC http.Handler

	// Faults fails requests on purpose. Nil fails nothing.
	Faults *faults.Set

	// Services hosts deployed Cloud Run services. Nil means none are served.
	Services ServiceHost
}

// ServiceHost resolves a request to a running Cloud Run service. It is the
// same shape as FunctionHost's Route: the front door knows only that
// something claimed the request.
type ServiceHost interface {
	Route(escapedPath string) (h http.Handler, rest string, ok bool)
}

// FunctionHost serves deployed functions and their admin API.
//
// Route takes the whole path rather than a name so that URL grammar — the
// project and location prefix among it — stays in the functions package. The
// front door knows only that something claimed the request.
type FunctionHost interface {
	// Route resolves a request path to a function and the path it should see.
	Route(escapedPath string) (h http.Handler, rest string, ok bool)

	// Admin serves the deploy, list, describe and delete API.
	Admin() http.Handler
}

// RunnerInfo separates what was asked for from what is in force, so health
// stays honest while no runner exists.
type RunnerInfo struct {
	Configured string `json:"configured"`
	Mode       string `json:"mode"`
}

// Handler is the h2c front door.
type Handler struct {
	rest     Router
	clk      clock.Clock
	started  time.Time
	version  string
	runner   RunnerInfo
	fns      FunctionHost
	mounts   map[string]http.Handler
	fallback http.Handler
	reset    func(context.Context, string) error
	grpc     http.Handler
	faults   *faults.Set
	services ServiceHost
}

// New builds the front door. Routes register here so the endpoint set is
// readable in one place.
func New(cfg Config) *Handler {
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	runner := cfg.Runner
	if runner.Configured == "" {
		runner.Configured = "none"
	}
	if runner.Mode == "" {
		runner.Mode = "none"
	}

	h := &Handler{
		clk:      cfg.Clock,
		started:  cfg.Clock.Now(),
		version:  version,
		runner:   runner,
		fns:      cfg.Functions,
		mounts:   cfg.Mounts,
		fallback: cfg.Fallback,
		grpc:     cfg.GRPC,
		faults:   cfg.Faults,
		services: cfg.Services,
	}
	h.rest.Handle(http.MethodGet, "/_emu/health", h.health)
	if cfg.Reset != nil {
		h.reset = cfg.Reset
		h.rest.Handle(http.MethodPost, "/_emu/reset", h.handleReset)
	}
	return h
}

// Protocols is the set every cloudrig listener uses: HTTP/1.1 plus cleartext
// HTTP/2. Go 1.24 serves h2c from net/http, so this costs no dependency.
func Protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// ServeHTTP splits gRPC from REST. Real clients send a subtype
// (application/grpc+proto), hence the prefix test rather than equality.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		h.serveGRPC(w, r)
		return
	}
	path := r.URL.EscapedPath()

	// Before any routing: a fault stands in for whatever would have answered,
	// so a rule can fail a path no service claims as readily as one it does.
	// The admin API is exempt, or a broad rule would lock out its own switch.
	if h.faults != nil && !strings.HasPrefix(path, adminPrefix) {
		if rule, ok := h.faults.Match(r.Method, path); ok {
			h.inject(w, r, rule)
			return
		}
	}

	if h.fns != nil && strings.HasPrefix(path, functionAdminPath) {
		h.fns.Admin().ServeHTTP(w, r)
		return
	}
	for prefix, mount := range h.mounts {
		if strings.HasPrefix(path, prefix) {
			mount.ServeHTTP(w, r)
			return
		}
	}
	if h.fns != nil {
		if fn, rest, ok := h.fns.Route(path); ok {
			serveFunction(fn, rest, w, r)
			return
		}
	}
	// Cloud Run services are addressed the same way, and tried after
	// functions: a name deployed as both is a function first, which is the
	// older behaviour.
	if h.services != nil {
		if svc, rest, ok := h.services.Route(path); ok {
			serveFunction(svc, rest, w, r)
			return
		}
	}
	if h.fallback != nil && !h.rest.Matches(r.Method, path) {
		h.fallback.ServeHTTP(w, r)
		return
	}
	h.rest.ServeHTTP(w, r)
}

// handleReset clears state, optionally scoped to one project.
func (h *Handler) handleReset(w http.ResponseWriter, r *http.Request, _ Params) error {
	if err := h.reset(r.Context(), r.URL.Query().Get("project")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// functionAdminPath duplicates functions.AdminPath rather than importing it,
// so the front door does not depend on a service package.
const functionAdminPath = "/_emu/functions"

// serveFunction rewrites the path to what the function sees: a function
// deployed as "hello" is the root of its own URL space, so /hello/a reaches it
// as /a, matching how Cloud Functions presents it.
func serveFunction(fn http.Handler, rest string, w http.ResponseWriter, r *http.Request) {
	sub := r.Clone(r.Context())
	sub.URL = new(url.URL)
	*sub.URL = *r.URL
	sub.URL.Path = mustUnescape(rest)
	sub.URL.RawPath = rest
	fn.ServeHTTP(w, sub)
}

func mustUnescape(p string) string {
	unescaped, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return unescaped
}

// serveGRPC hands the request to the gRPC server, or answers 501 when none is
// registered. 501 is the honest answer rather than a placeholder: the gRPC
// HTTP/2 spec maps it onto UNIMPLEMENTED, so a real client reads it right.
func (h *Handler) serveGRPC(w http.ResponseWriter, r *http.Request) {
	if h.grpc != nil {
		h.grpc.ServeHTTP(w, r)
		return
	}
	gerr.WriteJSON(w, gerr.NewUnimplemented(
		"gRPC "+r.URL.EscapedPath()+": no gRPC services are registered"))
}

// adminPrefix is the emulator's own control surface, which faults never touch.
const adminPrefix = "/_emu/"

// inject answers a request with a fault instead of routing it.
//
// The latency runs on the injected clock: under a FakeClock the request waits
// until a test advances time, which is what makes a slow backend something a
// test can assert on rather than sit through.
func (h *Handler) inject(w http.ResponseWriter, r *http.Request, rule faults.Rule) {
	if rule.Latency > 0 && !h.sleep(r, rule.Latency) {
		return // the client gave up first
	}
	gerr.WriteJSON(w, rule.Err())
}

// sleep waits on the clock, reporting whether it elapsed rather than the
// request being cancelled.
func (h *Handler) sleep(r *http.Request, d time.Duration) bool {
	done := make(chan struct{})
	timer := h.clk.AfterFunc(d, func() { close(done) })

	select {
	case <-done:
		return true
	case <-r.Context().Done():
		timer.Stop()
		return false
	}
}
