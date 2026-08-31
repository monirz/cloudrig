package faults

import (
	"net/http"
	"testing"

	"github.com/monirz/cloudrig/core/gerr"
)

func TestRuleMatching(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		rule   Rule
		method string
		path   string
		want   bool
	}{
		{"an empty rule matches anything", Rule{}, "GET", "/anything", true},
		{"an exact path", Rule{Path: "/storage/v1/b"}, "GET", "/storage/v1/b", true},
		{"an exact path, another request", Rule{Path: "/storage/v1/b"}, "GET", "/storage/v1/o", false},
		{"a prefix", Rule{Path: "/storage/v1/*"}, "GET", "/storage/v1/b/x/o", true},
		{"a prefix, another service", Rule{Path: "/storage/v1/*"}, "GET", "/v1/projects/p", false},
		{"the method too", Rule{Method: "POST", Path: "/x"}, "POST", "/x", true},
		{"the wrong method", Rule{Method: "POST", Path: "/x"}, "GET", "/x", false},
		{"the method, case-insensitively", Rule{Method: "post"}, "POST", "/x", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rule.matches(c.method, c.path); got != c.want {
				t.Errorf("matches(%q, %q) = %v, want %v", c.method, c.path, got, c.want)
			}
		})
	}
}

// TestCountIsConsumed is what makes a retry testable: fail once, then let the
// retry through.
func TestCountIsConsumed(t *testing.T) {
	t.Parallel()

	s := New().Add(Rule{Path: "/x", Count: 2})

	for i := 1; i <= 2; i++ {
		if _, ok := s.Match("GET", "/x"); !ok {
			t.Fatalf("request %d was not failed", i)
		}
	}
	if _, ok := s.Match("GET", "/x"); ok {
		t.Error("the rule fired a third time, past its count")
	}
	if s.Len() != 0 {
		t.Errorf("an exhausted rule is still armed: %d left", s.Len())
	}
}

// TestZeroCountIsUnlimited covers the other half: a rule with no count keeps
// failing until it is cleared.
func TestZeroCountIsUnlimited(t *testing.T) {
	t.Parallel()

	s := New().Add(Rule{Path: "/x"})
	for i := 0; i < 50; i++ {
		if _, ok := s.Match("GET", "/x"); !ok {
			t.Fatalf("stopped firing after %d", i)
		}
	}
	s.Clear()
	if _, ok := s.Match("GET", "/x"); ok {
		t.Error("a cleared rule still fired")
	}
}

// TestFirstRuleWins pins the order, so a narrow rule added before a broad one
// is the one that answers.
func TestFirstRuleWins(t *testing.T) {
	t.Parallel()

	s := New().
		Add(Rule{Path: "/x", Status: http.StatusNotFound}).
		Add(Rule{Status: http.StatusServiceUnavailable})

	got, ok := s.Match("GET", "/x")
	if !ok || got.status() != http.StatusNotFound {
		t.Errorf("status = %d, want 404 from the first rule", got.status())
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()

	err := Rule{}.Err()
	var g *gerr.Error
	if !asError(err, &g) {
		t.Fatalf("Err() = %T, want a *gerr.Error", err)
	}
	if g.HTTPStatus() != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", g.HTTPStatus())
	}
	if g.Code != gerr.Unavailable {
		t.Errorf("code = %v, want Unavailable", g.Code)
	}

	// A status the caller gave without a code gets a matching one.
	rated := Rule{Status: http.StatusTooManyRequests}.Err()
	asError(rated, &g)
	if g.Code != gerr.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted for a 429", g.Code)
	}
}

// asError is errors.As, spelled locally to keep the test's imports honest
// about what it is really asserting.
func asError(err error, into **gerr.Error) bool {
	g, ok := err.(*gerr.Error)
	if ok {
		*into = g
	}
	return ok
}
