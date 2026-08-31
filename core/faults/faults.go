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

// Err renders the rule as the error the transport writes.
func (r Rule) Err() error {
	msg := r.Message
	if msg == "" {
		msg = "injected fault"
	}
	code := r.Code
	if code == 0 {
		code = codeFor(r.status())
	}
	return gerr.New(code, msg).WithHTTPStatus(r.status())
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
	rule Rule
	left int // remaining firings; -1 is unlimited
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
