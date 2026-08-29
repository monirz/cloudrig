// Package transport is cloudrig's single front door: one handler, one port,
// serving REST over HTTP/1.1 and gRPC over cleartext HTTP/2 at the same time.
package transport

import (
	"net/http"
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

	// Runner describes the function runner. Step 1 ships no runner, so this
	// reports honestly rather than aspirationally.
	Runner RunnerInfo
}

// RunnerInfo is the runner's configured mode and what it actually resolved to.
// Configured is what the user asked for ("auto"); Mode is what is really in
// force ("none", until a runner exists).
type RunnerInfo struct {
	Configured string `json:"configured"`
	Mode       string `json:"mode"`
}

// Handler is the h2c front door.
type Handler struct {
	rest    router
	clk     clock.Clock
	started time.Time
	version string
	runner  RunnerInfo
}

// New builds the front door. Routes are registered here so that the set of
// endpoints is readable in one place.
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
		clk:     cfg.Clock,
		started: cfg.Clock.Now(),
		version: version,
		runner:  runner,
	}
	h.rest.handle(http.MethodGet, "/_emu/health", h.health)
	return h
}

// Protocols is the protocol set every cloudrig listener must use: HTTP/1.1 and
// cleartext HTTP/2 on the same port.
//
// Go 1.24 serves h2c from net/http directly, so this needs no dependency —
// notably not golang.org/x/net/http2/h2c, which was the only way to do it
// before. Both cmd/cloudrig and the library entry point call this, so there is
// exactly one place that decides the port speaks h2c.
func Protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// ServeHTTP dispatches gRPC to the gRPC branch and everything else to the REST
// mux. gRPC is HTTP/2 with an application/grpc content type; the content type
// carries a subtype in practice (application/grpc+proto), hence the prefix test.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		h.serveGRPC(w, r)
		return
	}
	h.rest.serve(w, r)
}

// serveGRPC answers every gRPC request with 501 until a real service is
// registered.
//
// This is not a stub for a grpc.Server that will be dropped in later — an
// unused grpc.NewServer would be a dependency with no test. What must survive
// is the shape: the h2c listener, the content-type discrimination, and this
// dispatch point. gRPC arrives here together with a service that exercises it.
//
// 501 is also the honest answer rather than a placeholder: the gRPC HTTP/2
// specification maps HTTP status 501 onto the UNIMPLEMENTED code, so a real
// gRPC client reads this exactly as intended without any trailer handling.
func (h *Handler) serveGRPC(w http.ResponseWriter, r *http.Request) {
	gerr.WriteJSON(w, gerr.NewUnimplemented(
		"gRPC "+r.URL.EscapedPath()+": no gRPC services are registered"))
}
