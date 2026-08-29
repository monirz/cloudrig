// Package transport is cloudrig's front door: one handler on one port, serving
// REST over HTTP/1.1 and gRPC over cleartext HTTP/2 at once.
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

	// Runner describes the function runner. No runner exists yet.
	Runner RunnerInfo
}

// RunnerInfo separates what was asked for from what is in force, so health
// stays honest while no runner exists.
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
		clk:     cfg.Clock,
		started: cfg.Clock.Now(),
		version: version,
		runner:  runner,
	}
	h.rest.handle(http.MethodGet, "/_emu/health", h.health)
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
	h.rest.serve(w, r)
}

// serveGRPC answers 501 until a service is registered. Not a placeholder: the
// gRPC HTTP/2 spec maps 501 onto UNIMPLEMENTED, so real clients read it right.
func (h *Handler) serveGRPC(w http.ResponseWriter, r *http.Request) {
	gerr.WriteJSON(w, gerr.NewUnimplemented(
		"gRPC "+r.URL.EscapedPath()+": no gRPC services are registered"))
}
