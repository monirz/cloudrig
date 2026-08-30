package transport

// ExportRoute registers a route from an external test. Registration is
// unexported, but the matcher needs proving before a service provides a route.
func ExportRoute(h *Handler, method, pattern string, fn HandlerFunc) {
	h.rest.Handle(method, pattern, fn)
}
