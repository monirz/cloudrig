package functions

import (
	"net/http"
	"net/url"
	"strings"
)

// Route resolves a request path to a deployed function and the path that
// function should see.
//
// Two forms are accepted:
//
//	/{name}                          the default project and location
//	/{location}-{project}/{name}     the form the GCF emulator uses
//
// A function is the root of its own URL space, so /hello/a/b reaches it as
// /a/b, matching how Cloud Functions presents it.
func (r *Registry) Route(escapedPath string) (http.Handler, string, bool) {
	first, rest := cut(strings.TrimPrefix(escapedPath, "/"))

	// The prefixed form is tried first: a bare name cannot contain a slash, so
	// a two-segment path is only ambiguous if a function is literally named
	// like a location-project pair, and the prefixed reading is the intended
	// one there anyway.
	if location, project, ok := splitPrefix(first); ok {
		name, sub := cut(rest)
		if h, found := r.Handler(project, location, unescape(name)); found {
			return h, "/" + sub, true
		}
	}

	if h, found := r.Handler("", "", unescape(first)); found {
		return h, "/" + rest, true
	}
	return nil, "", false
}

// splitPrefix reads a {location}-{project} segment. Locations are of the form
// us-central1, so the project starts after the third hyphen.
func splitPrefix(seg string) (location, project string, ok bool) {
	parts := strings.SplitN(seg, "-", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0] + "-" + parts[1], parts[2], true
}

func cut(p string) (head, rest string) {
	head, rest, _ = strings.Cut(p, "/")
	return head, rest
}

func unescape(s string) string {
	out, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return out
}
