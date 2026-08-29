package transport

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
)

// Params holds the decoded capture values from a matched route.
type Params map[string]string

// HandlerFunc is a route handler. Returning an error is the normal way to fail:
// the router renders it through gerr, so no handler has to remember to write an
// envelope by hand.
type HandlerFunc func(w http.ResponseWriter, r *http.Request, p Params) error

// router matches on r.URL.EscapedPath(), never r.URL.Path.
//
// net/http decodes Path before a handler sees it, which makes a%2Fb and a/b
// indistinguishable — and GCP resource segments arrive percent-encoded, so an
// object literally named "logs/app.log" is exactly the case that breaks. We
// split the escaped path on "/" and unescape each captured segment ourselves,
// so a %2F inside a segment stays inside that segment.
type router struct {
	routes []route
}

type route struct {
	method string
	segs   []segment
	h      HandlerFunc
}

type segment struct {
	literal string // matched raw, against the still-escaped segment
	capture string // non-empty when this segment is a {name} capture
}

// handle registers a route. Pattern segments wrapped in braces capture that one
// segment; everything else matches literally. There is deliberately no
// multi-segment wildcard: nothing needs one, and a matcher nothing exercises is
// a matcher that rots.
func (rt *router) handle(method, pattern string, h HandlerFunc) {
	raw := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	segs := make([]segment, len(raw))
	for i, s := range raw {
		if len(s) > 2 && strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = segment{capture: s[1 : len(s)-1]}
			continue
		}
		segs[i] = segment{literal: s}
	}
	rt.routes = append(rt.routes, route{method: method, segs: segs, h: h})
}

// match finds a route for the request. It reports pathOK separately so the
// caller can tell 404 from 405: a path that exists under a different method is
// a different failure than one that does not exist.
func (rt *router) match(method, escapedPath string) (r *route, p Params, pathOK bool) {
	got := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")

	for i := range rt.routes {
		rr := &rt.routes[i]
		params, ok := rr.matchPath(got)
		if !ok {
			continue
		}
		pathOK = true
		if rr.method == method {
			return rr, params, true
		}
	}
	return nil, nil, pathOK
}

func (rr *route) matchPath(got []string) (Params, bool) {
	if len(got) != len(rr.segs) {
		return nil, false
	}
	var p Params
	for i, seg := range rr.segs {
		if seg.capture == "" {
			if got[i] != seg.literal {
				return nil, false
			}
			continue
		}
		// Unescape only here, one segment at a time. PathUnescape leaves an
		// encoded slash as a literal slash inside this value rather than
		// letting it split the path.
		val, err := url.PathUnescape(got[i])
		if err != nil {
			return nil, false
		}
		if p == nil {
			p = make(Params, len(rr.segs))
		}
		p[seg.capture] = val
	}
	return p, true
}

func (rt *router) serve(w http.ResponseWriter, r *http.Request) {
	rr, params, pathOK := rt.match(r.Method, r.URL.EscapedPath())
	switch {
	case rr != nil:
		if err := rr.h(w, r, params); err != nil {
			gerr.WriteJSON(w, err)
		}
	case pathOK:
		gerr.WriteJSON(w, gerr.Newf(gerr.InvalidArgument,
			"method %s not allowed on %s", r.Method, r.URL.EscapedPath()).
			WithHTTPStatus(http.StatusMethodNotAllowed).
			WithReason("methodNotAllowed"))
	default:
		gerr.WriteJSON(w, gerr.Newf(gerr.NotFound,
			"no route for %s %s", r.Method, r.URL.EscapedPath()).
			WithHTTPStatus(http.StatusNotFound).
			WithReason("notFound"))
	}
}
