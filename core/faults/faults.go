// Package faults injects failures into the emulator's responses.
//
// It exists so a test can prove its own error handling: a retry loop is only
// tested by a request that actually fails, and against a real cloud a failure
// is something you wait for rather than ask for.
package faults

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/monirz/cloudrig/core/gerr"
)

// Rule decides which requests fail and how. A zero Method or Path matches
// anything, so the empty Rule matches every request.
type Rule struct {
	// Method is an HTTP method, or empty for any.
	Method string

	// Path matches the escaped path. A trailing * makes it a prefix, so
	// "/storage/v1/*" covers a whole service.
	Path string

	// Status is the HTTP status to answer with. Zero means 503.
	Status int

	// Code is the canonical error code. Zero is derived from Status.
	Code gerr.Code

	// Message is the error text. Empty gets a default naming the rule.
	Message string

	// Latency delays the response. It runs on the injected clock, so under a
	// FakeClock the request waits until a test advances time.
	Latency time.Duration

	// Count is how many matching requests to fail. Zero means every one.
	Count int

	// Rate is the fraction of matching requests to fail, 0..1. Zero means every
	// one. It is applied deterministically — a rate of 0.2 fails exactly one in
	// every five matching requests, in a fixed pattern — so a fault composes
	// with time travel into a reproducible test rather than a coin flip.
	Rate float64
}

// matches reports whether a request falls under this rule.
func (r Rule) matches(method, path string) bool {
	if r.Method != "" && !strings.EqualFold(r.Method, method) {
		return false
	}
	switch {
	case r.Path == "":
		return true
	case strings.HasSuffix(r.Path, "*"):
		return strings.HasPrefix(path, strings.TrimSuffix(r.Path, "*"))
	default:
		return r.Path == path
	}
}

// status is the HTTP status this rule answers with.
func (r Rule) status() int {
	if r.Status == 0 {
		return http.StatusServiceUnavailable
	}
	return r.Status
}

// message is the error text, defaulted.
func (r Rule) message() string {
	if r.Message == "" {
		return "injected fault"
	}
	return r.Message
}

// code is the canonical (gRPC-numbered) code the rule answers with, derived
// from the status when not set explicitly.
func (r Rule) code() gerr.Code {
	if r.Code != 0 {
		return r.Code
	}
	return codeFor(r.status())
}

// FailsRequest reports whether a matched rule ends the request with an error.
// A latency-only rule — a delay with no error asked for — does not: it slows a
// request that then succeeds normally. Every other rule (an explicit status, or
// the bare rule with neither latency nor status) fails the request.
func (r Rule) FailsRequest() bool {
	return r.Status != 0 || r.Code != 0 || r.Latency == 0
}

// Err renders the rule as the error the transport writes over REST.
func (r Rule) Err() error {
	return gerr.New(r.code(), r.message()).WithHTTPStatus(r.status())
}

// codeFor picks a canonical code for a status the caller did not pair one
// with. gerr maps the other direction; this covers only the statuses a fault
// is plausibly asked for.
func codeFor(status int) gerr.Code {
	switch status {
	case http.StatusBadRequest:
		return gerr.InvalidArgument
	case http.StatusUnauthorized:
		return gerr.Unauthenticated
	case http.StatusForbidden:
		return gerr.PermissionDenied
	case http.StatusNotFound:
		return gerr.NotFound
	case http.StatusConflict:
		return gerr.Aborted
	case http.StatusTooManyRequests:
		return gerr.ResourceExhausted
	case http.StatusInternalServerError:
		return gerr.Internal
	case http.StatusNotImplemented:
		return gerr.Unimplemented
	case http.StatusGatewayTimeout:
		return gerr.DeadlineExceeded
	}
	return gerr.Unavailable
}

// Set is the live list of rules. It is safe for concurrent use: rules are
// added from a test goroutine while requests are being served.
type Set struct {
	mu    sync.Mutex
	rules []*live
}

type live struct {
	rule   Rule
	left   int // remaining firings; -1 is unlimited
	seen   int // matching requests observed, for the rate gate
	failed int // of those, how many were failed
}

// New returns an empty Set, which fails nothing.
func New() *Set { return &Set{} }

// Add arms a rule and returns the Set, so calls can be chained.
func (s *Set) Add(r Rule) *Set {
	left := r.Count
	if left <= 0 {
		left = -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &live{rule: r, left: left})
	return s
}

// Clear disarms every rule.
func (s *Set) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = nil
}

// Len reports how many rules are still armed, so a test can assert one fired
// as often as it expected.
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rules)
}

// Match returns the first rule claiming a request, consuming one of its
// firings. Exhausted rules are dropped, so a Count of 1 fails once and then
// lets the retry through.
func (s *Set) Match(method, path string) (Rule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, l := range s.rules {
		if !l.rule.matches(method, path) {
			continue
		}
		l.seen++
		// Rate gate: let a deterministic fraction through. A rate of 0.2 fails
		// when the running count of expected failures (seen*rate, floored)
		// overtakes how many we have failed — one in five, evenly, repeatably.
		if r := l.rule.Rate; r > 0 && int(float64(l.seen)*r) <= l.failed {
			return Rule{}, false
		}
		l.failed++
		if l.left > 0 {
			l.left--
			if l.left == 0 {
				s.rules = append(s.rules[:i], s.rules[i+1:]...)
			}
		}
		return l.rule, true
	}
	return Rule{}, false
}

// Rules returns a snapshot of the armed rules, for a status view. Order is arm
// order, which is match order.
func (s *Set) Rules() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Rule, len(s.rules))
	for i, l := range s.rules {
		out[i] = l.rule
	}
	return out
}
