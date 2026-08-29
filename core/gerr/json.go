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
type Envelope struct {
	Error EnvelopeError `json:"error"`
}

// EnvelopeError is the "error" object. Code is the HTTP status; the canonical
// code appears as Status.
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

// Envelope renders the error into the JSON body. Reason and Location produce
// the primary errors[] entry; each detail violation appends another.
func (e *Error) Envelope() Envelope {
	domain := e.Domain
	reason := e.Reason
	for _, d := range e.Details {
		info, ok := d.(ErrorInfo)
		if !ok {
			continue
		}
		// ErrorInfo only fills gaps, so a handler that set both wins.
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

// WriteJSON renders err as the GCP JSON error envelope, coercing through From
// so no handler can emit a body of another shape.
func WriteJSON(w http.ResponseWriter, err error) {
	e := From(err)
	if e == nil {
		e = New(Internal, "nil error")
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.HTTPStatus())
	// Status is already committed; Envelope holds no unmarshalable types.
	_ = json.NewEncoder(w).Encode(e.Envelope())
}

// lowerCamel converts CONDITION_NOT_MET to conditionNotMet, passing an already
// lowerCamel reason through untouched.
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
