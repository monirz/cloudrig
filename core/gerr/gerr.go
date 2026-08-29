package gerr

import (
	"errors"
	"fmt"
)

// Error is a canonical error. Reason and Location are first-class because
// clients branch on errors[].reason, not on a generic detail map.
type Error struct {
	Code    Code
	Message string

	// Reason is the lowerCamel string clients branch on: "conditionNotMet".
	Reason string

	// Location names the request element at fault; LocationType says which
	// kind it is ("header", "parameter").
	Location     string
	LocationType string

	// Domain defaults to "global" when rendered, matching GCS.
	Domain string

	Details []Detail

	cause error

	// httpStatus is 0 until set explicitly; HTTPStatusIsExplicit reports which.
	httpStatus int
}

// New returns an error carrying the canonical HTTP status for code. Service
// handlers should chain WithHTTPStatus rather than rely on that default.
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Newf is New with formatting.
func Newf(code Code, format string, a ...any) *Error {
	return New(code, fmt.Sprintf(format, a...))
}

// Wrap annotates err, preserving it for errors.Is and errors.As.
func Wrap(err error, code Code, format string, a ...any) *Error {
	e := Newf(code, format, a...)
	e.cause = err
	return e
}

// From coerces any error into an *Error. An unclassified one becomes Internal:
// a failure we did not classify is our bug, not the caller's.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(err, Internal, "%s", err.Error())
}

// NewUnimplemented names the operation: a 501 that does not say what is
// missing is not loud enough (spec.md rule 5).
func NewUnimplemented(op string) *Error {
	return Newf(Unimplemented, "operation not implemented: %s", op).
		WithHTTPStatus(501).
		WithReason("notImplemented")
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// Is matches on code alone: errors.Is(err, gerr.New(gerr.NotFound, "")).
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// WithHTTPStatus states the status this error renders as. Service handlers
// call it at every user-visible construction site.
func (e *Error) WithHTTPStatus(status int) *Error {
	e.httpStatus = status
	return e
}

// WithReason sets the string clients branch on.
func (e *Error) WithReason(reason string) *Error {
	e.Reason = reason
	return e
}

// WithLocation names the request element at fault; locationType is "header",
// "parameter" or "".
func (e *Error) WithLocation(location, locationType string) *Error {
	e.Location, e.LocationType = location, locationType
	return e
}

// WithDomain overrides the default "global".
func (e *Error) WithDomain(domain string) *Error {
	e.Domain = domain
	return e
}

// With appends google.rpc detail payloads.
func (e *Error) With(details ...Detail) *Error {
	e.Details = append(e.Details, details...)
	return e
}

// HTTPStatus is the explicit status if set, else the canonical mapping.
func (e *Error) HTTPStatus() int {
	if e.httpStatus != 0 {
		return e.httpStatus
	}
	if s, ok := canonicalHTTP[e.Code]; ok {
		return s
	}
	return 500
}

// HTTPStatusIsExplicit reports whether WithHTTPStatus was called, so a
// service-layer check can fail the build on a handler that forgot.
func (e *Error) HTTPStatusIsExplicit() bool { return e.httpStatus != 0 }
