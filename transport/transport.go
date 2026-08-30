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

// serveGRPC answers 501 until a service is registered. Not a placeholder: the
// gRPC HTTP/2 spec maps 501 onto UNIMPLEMENTED, so real clients read it right.
func (h *Handler) serveGRPC(w http.ResponseWriter, r *http.Request) {
	gerr.WriteJSON(w, gerr.NewUnimplemented(
		"gRPC "+r.URL.EscapedPath()+": no gRPC services are registered"))
}
