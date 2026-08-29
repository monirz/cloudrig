package gerr

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
)

// Envelope is the GCP JSON error body:
//
//	{"error":{"code":412,"message":"...","errors":[...],"status":"FAILED_PRECONDITION"}}
//
// It is exported so tests can assert on the structure without going through an
// HTTP round trip.
type Envelope struct {
	Error EnvelopeError `json:"error"`
}

// EnvelopeError is the "error" object. Code is the HTTP status, not the
// canonical code — the canonical code appears as Status.
type EnvelopeError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Errors  []EnvelopeEntry `json:"errors,omitempty"`
	Status  string          `json:"status"`
}

// EnvelopeEntry is one entry in errors[]. Clients branch on Reason.
type EnvelopeEntry struct {
	Message      string `json:"message,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Location     string `json:"location,omitempty"`
	LocationType string `json:"locationType,omitempty"`
}

const defaultDomain = "global"

// Envelope renders the error into the JSON body shape.
//
// The flat errors[] array is where the structured google.rpc details collapse.
// The first-class Reason and Location produce the primary entry; each detail
// violation appends another. An ErrorInfo fills in Reason and Domain only when
// the first-class fields left them empty, so a handler that set both does not
// get a contradictory duplicate.
func (e *Error) Envelope() Envelope {
	domain := e.Domain
	reason := e.Reason
	for _, d := range e.Details {
		info, ok := d.(ErrorInfo)
		if !ok {
			continue
		}
		if reason == "" {
			reason = lowerCamel(info.Reason)
		}
		if domain == "" {
			domain = info.Domain
		}
	}
	if domain == "" {
		domain = defaultDomain
	}

	var entries []EnvelopeEntry
	if reason != "" || e.Location != "" {
		entries = append(entries, EnvelopeEntry{
			Message:      e.Message,
			Domain:       domain,
			Reason:       reason,
			Location:     e.Location,
			LocationType: e.LocationType,
		})
	}

	for _, d := range e.Details {
		switch v := d.(type) {
		case BadRequest:
			for _, fv := range v.FieldViolations {
				entries = append(entries, EnvelopeEntry{
					Message:      fv.Description,
					Domain:       domain,
					Reason:       "invalid",
					Location:     fv.Field,
					LocationType: "parameter",
				})
			}
		case PreconditionFailure:
			for _, pv := range v.Violations {
				entries = append(entries, EnvelopeEntry{
					Message:  pv.Description,
					Domain:   domain,
					Reason:   lowerCamel(pv.Type),
					Location: pv.Subject,
				})
			}
		}
	}

	return Envelope{Error: EnvelopeError{
		Code:    e.HTTPStatus(),
		Message: e.Message,
		Errors:  entries,
		Status:  e.Code.String(),
	}}
}

// WriteJSON renders err as the GCP JSON error envelope. A nil error, or one
// that is not a *Error, is coerced through From — a handler must never be able
// to emit a body that is not this shape.
func WriteJSON(w http.ResponseWriter, err error) {
	e := From(err)
	if e == nil {
		e = New(Internal, "nil error")
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.HTTPStatus())
	// The status line is already committed, so a marshal failure here can only
	// be logged, not reported. Envelope contains no unmarshalable types.
	_ = json.NewEncoder(w).Encode(e.Envelope())
}

// lowerCamel converts an UPPER_SNAKE proto reason to the lowerCamel spelling
// the JSON envelope uses: CONDITION_NOT_MET -> conditionNotMet. A reason that
// is already lowerCamel passes through untouched.
func lowerCamel(s string) string {
	if s == "" || !strings.ContainsRune(s, '_') {
		return s
	}
	var b strings.Builder
	upper := false
	for i, r := range strings.ToLower(s) {
		switch {
		case r == '_':
			upper = true
		case upper && i > 0:
			b.WriteRune(unicode.ToUpper(r))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
