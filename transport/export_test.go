package transport

// ExportRoute registers a route from an external test. Route registration is
// unexported because the endpoint set belongs in New, where it is readable in
// one place; the escaped-path matcher still has to be provable against
// arbitrary patterns, and acceptance criterion 4 needs a route with a capture
// before any service exists to provide one.
func ExportRoute(h *Handler, method, pattern string, fn HandlerFunc) {
	h.rest.handle(method, pattern, fn)
}
