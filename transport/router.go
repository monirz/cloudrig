package transport

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
)

// Params holds the decoded capture values from a matched route.
type Params map[string]string

// HandlerFunc is a route handler. Returning an error is the normal failure
// path: the router renders it through gerr.
type HandlerFunc func(w http.ResponseWriter, r *http.Request, p Params) error

// Router matches on EscapedPath, never Path: net/http decodes Path first,
// which makes a%2Fb and a/b indistinguishable. GCP resource names need both.
//
// It is exported so service packages can declare their own routes without the
// front door having to know about them.
type Router struct {
	routes []route
}

// NewRouter returns an empty Router.
func NewRouter() *Router { return &Router{} }

type route struct {
	method string
	segs   []segment
	h      HandlerFunc
}

type segment struct {
	literal string // matched raw, against the still-escaped segment
	capture string // non-empty when this segment is a {name} capture
}

// Handle registers a route. Braced segments capture; everything else matches
// literally. There is no multi-segment wildcard because nothing needs one.
func (rt *Router) Handle(method, pattern string, h HandlerFunc) {
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

// match finds a route, reporting pathOK separately so the caller can tell 404
// from 405.
func (rt *Router) match(method, escapedPath string) (r *route, p Params, pathOK bool) {
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
		// Unescape one segment at a time, so a %2F stays inside this value.
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

// ServeHTTP dispatches to a matching route, rendering a handler's error and
// distinguishing 404 from 405.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
