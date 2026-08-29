package gerr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monirz/cloudrig/internal/core/gerr"
)

func TestHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          *gerr.Error
		want         int
		wantExplicit bool
	}{
		{
			name: "canonical fallback",
			err:  gerr.New(gerr.NotFound, "no such bucket"),
			want: 404,
		},
		{
			// The case the whole override mechanism exists for: the canonical
			// table says 400, GCS says 412.
			name:         "explicit status wins over the canonical table",
			err:          gerr.New(gerr.FailedPrecondition, "condition not met").WithHTTPStatus(412),
			want:         412,
			wantExplicit: true,
		},
		{
			name: "canonical FailedPrecondition is 400, not 412",
			err:  gerr.New(gerr.FailedPrecondition, "condition not met"),
			want: 400,
		},
		{
			name:         "unimplemented is explicit",
			err:          gerr.NewUnimplemented("storage.objects.rewrite"),
			want:         501,
			wantExplicit: true,
		},
		{
			name: "unknown code falls back to 500",
			err:  gerr.New(gerr.Code(99), "who knows"),
			want: 500,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.HTTPStatus(); got != tc.want {
				t.Errorf("HTTPStatus() = %d, want %d", got, tc.want)
			}
			if got := tc.err.HTTPStatusIsExplicit(); got != tc.wantExplicit {
				t.Errorf("HTTPStatusIsExplicit() = %v, want %v", got, tc.wantExplicit)
			}
		})
	}
}

func TestCodeString(t *testing.T) {
	t.Parallel()

	// The envelope's "status" field is these names verbatim, so the spelling is
	// load-bearing — note Google's CANCELLED.
	tests := map[gerr.Code]string{
		gerr.OK:                 "OK",
		gerr.Canceled:           "CANCELLED",
		gerr.NotFound:           "NOT_FOUND",
		gerr.FailedPrecondition: "FAILED_PRECONDITION",
		gerr.Unimplemented:      "UNIMPLEMENTED",
		gerr.Code(99):           "UNKNOWN",
	}
	for code, want := range tests {
		if got := code.String(); got != want {
			t.Errorf("Code(%d).String() = %q, want %q", code, got, want)
		}
	}
}

func TestCodeValuesMatchGoogleRPC(t *testing.T) {
	t.Parallel()

	// A later gRPC rendering is meant to be a cast, not a translation. If these
	// drift, that stops being true silently.
	want := map[gerr.Code]int32{
		gerr.OK: 0, gerr.Canceled: 1, gerr.Unknown: 2, gerr.InvalidArgument: 3,
		gerr.DeadlineExceeded: 4, gerr.NotFound: 5, gerr.AlreadyExists: 6,
		gerr.PermissionDenied: 7, gerr.ResourceExhausted: 8,
		gerr.FailedPrecondition: 9, gerr.Aborted: 10, gerr.OutOfRange: 11,
		gerr.Unimplemented: 12, gerr.Internal: 13, gerr.Unavailable: 14,
		gerr.DataLoss: 15, gerr.Unauthenticated: 16,
	}
	for code, n := range want {
		if int32(code) != n {
			t.Errorf("%s = %d, want %d", code, int32(code), n)
		}
	}
}

func TestEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *gerr.Error
		want gerr.Envelope
	}{
		{
			name: "reason and location become the primary entry",
			err: gerr.New(gerr.FailedPrecondition, "condition not met").
				WithHTTPStatus(412).
				WithReason("conditionNotMet").
				WithLocation("If-Match", "header"),
			want: gerr.Envelope{Error: gerr.EnvelopeError{
				Code:    412,
				Message: "condition not met",
				Status:  "FAILED_PRECONDITION",
				Errors: []gerr.EnvelopeEntry{{
					Message: "condition not met", Domain: "global",
					Reason: "conditionNotMet", Location: "If-Match", LocationType: "header",
				}},
			}},
		},
		{
			name: "no reason and no location means no errors array",
			err:  gerr.New(gerr.Internal, "boom"),
			want: gerr.Envelope{Error: gerr.EnvelopeError{
				Code: 500, Message: "boom", Status: "INTERNAL",
			}},
		},
		{
			name: "ErrorInfo fills an empty reason, lowerCamel'd",
			err: gerr.New(gerr.NotFound, "no such object").
				WithHTTPStatus(404).
				With(gerr.ErrorInfo{
					Reason: "OBJECT_NOT_FOUND",
					Domain: "storage.googleapis.com",
				}),
			want: gerr.Envelope{Error: gerr.EnvelopeError{
				Code: 404, Message: "no such object", Status: "NOT_FOUND",
				Errors: []gerr.EnvelopeEntry{{
					Message: "no such object", Domain: "storage.googleapis.com",
					Reason: "objectNotFound",
				}},
			}},
		},
		{
			name: "a first-class reason is not overwritten by ErrorInfo",
			err: gerr.New(gerr.NotFound, "no such object").
				WithReason("handlerWins").
				With(gerr.ErrorInfo{Reason: "DETAIL_LOSES"}),
			want: gerr.Envelope{Error: gerr.EnvelopeError{
				Code: 404, Message: "no such object", Status: "NOT_FOUND",
				Errors: []gerr.EnvelopeEntry{{
					Message: "no such object", Domain: "global", Reason: "handlerWins",
				}},
			}},
		},
		{
			name: "field violations append entries",
			err: gerr.New(gerr.InvalidArgument, "bad request").
				WithHTTPStatus(400).
				With(gerr.BadRequest{FieldViolations: []gerr.FieldViolation{
					{Field: "maxResults", Description: "must be positive"},
					{Field: "prefix", Description: "must be valid UTF-8"},
				}}),
			want: gerr.Envelope{Error: gerr.EnvelopeError{
				Code: 400, Message: "bad request", Status: "INVALID_ARGUMENT",
				Errors: []gerr.EnvelopeEntry{
					{Message: "must be positive", Domain: "global", Reason: "invalid",
						Location: "maxResults", LocationType: "parameter"},
					{Message: "must be valid UTF-8", Domain: "global", Reason: "invalid",
						Location: "prefix", LocationType: "parameter"},
				},
			}},
		},
		{
			name: "precondition violations append entries",
			err: gerr.New(gerr.FailedPrecondition, "generation mismatch").
				WithHTTPStatus(412).
				With(gerr.PreconditionFailure{Violations: []gerr.PreconditionViolation{
					{Type: "GENERATION_MISMATCH", Subject: "b/bkt/o/x", Description: "expected 5"},
				}}),
			want: gerr.Envelope{Error: gerr.EnvelopeError{
				Code: 412, Message: "generation mismatch", Status: "FAILED_PRECONDITION",
				Errors: []gerr.EnvelopeEntry{
					{Message: "expected 5", Domain: "global",
						Reason: "generationMismatch", Location: "b/bkt/o/x"},
				},
			}},
		},
		{
			name: "an explicit domain overrides the default",
			err: gerr.New(gerr.NotFound, "gone").
				WithReason("notFound").
				WithDomain("storage.googleapis.com"),
			want: gerr.Envelope{Error: gerr.EnvelopeError{
				Code: 404, Message: "gone", Status: "NOT_FOUND",
				Errors: []gerr.EnvelopeEntry{{
					Message: "gone", Domain: "storage.googleapis.com", Reason: "notFound",
				}},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.err.Envelope()
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("Envelope()\n got = %+v\nwant = %+v", got, tc.want)
			}
		})
	}
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	t.Run("shape and headers", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		gerr.WriteJSON(rec, gerr.New(gerr.FailedPrecondition, "condition not met").
			WithHTTPStatus(412).
			WithReason("conditionNotMet"))

		if rec.Code != 412 {
			t.Errorf("status = %d, want 412", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=UTF-8" {
			t.Errorf("Content-Type = %q", ct)
		}

		// Decode into a generic map to prove the wire keys, not just the Go
		// struct: a client parses these names.
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("no error object in %s", rec.Body)
		}
		if errObj["code"] != float64(412) {
			t.Errorf("error.code = %v, want 412", errObj["code"])
		}
		if errObj["status"] != "FAILED_PRECONDITION" {
			t.Errorf("error.status = %v", errObj["status"])
		}
		entries, ok := errObj["errors"].([]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("error.errors = %v", errObj["errors"])
		}
		first := entries[0].(map[string]any)
		if first["reason"] != "conditionNotMet" {
			t.Errorf("errors[0].reason = %v", first["reason"])
		}
	})

	t.Run("a plain error becomes Internal", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		gerr.WriteJSON(rec, errors.New("something broke"))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		var env gerr.Envelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Status != "INTERNAL" || env.Error.Message != "something broke" {
			t.Errorf("envelope = %+v", env.Error)
		}
	})
}

func TestFromAndWrap(t *testing.T) {
	t.Parallel()

	t.Run("From passes an *Error through", func(t *testing.T) {
		t.Parallel()
		orig := gerr.New(gerr.NotFound, "gone")
		if got := gerr.From(orig); got != orig {
			t.Errorf("From returned a different value")
		}
	})

	t.Run("From finds a wrapped *Error", func(t *testing.T) {
		t.Parallel()
		orig := gerr.New(gerr.NotFound, "gone")
		wrapped := fmt.Errorf("while listing: %w", orig)
		if got := gerr.From(wrapped); got != orig {
			t.Errorf("From did not unwrap to the original")
		}
	})

	t.Run("From classifies a plain error as Internal", func(t *testing.T) {
		t.Parallel()
		got := gerr.From(errors.New("boom"))
		if got.Code != gerr.Internal {
			t.Errorf("code = %s, want INTERNAL", got.Code)
		}
	})

	t.Run("From of nil is nil", func(t *testing.T) {
		t.Parallel()
		if got := gerr.From(nil); got != nil {
			t.Errorf("From(nil) = %v, want nil", got)
		}
	})

	t.Run("Wrap preserves the cause for errors.Is", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("underlying")
		err := gerr.Wrap(sentinel, gerr.Internal, "while reading %s", "x")
		if !errors.Is(err, sentinel) {
			t.Error("errors.Is did not find the cause")
		}
		if want := "INTERNAL: while reading x: underlying"; err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
	})
}

func TestIsMatchesOnCode(t *testing.T) {
	t.Parallel()

	err := gerr.New(gerr.NotFound, "no such bucket")
	if !errors.Is(err, gerr.New(gerr.NotFound, "any message at all")) {
		t.Error("errors.Is did not match on code")
	}
	if errors.Is(err, gerr.New(gerr.AlreadyExists, "")) {
		t.Error("errors.Is matched a different code")
	}
}

func TestNewUnimplementedNamesTheOperation(t *testing.T) {
	t.Parallel()

	// Rule 5 in spec.md: a 501 that does not say what was not implemented is
	// not loud enough to be useful.
	err := gerr.NewUnimplemented("storage.objects.compose")
	if err.Message != "operation not implemented: storage.objects.compose" {
		t.Errorf("message = %q", err.Message)
	}
	if err.Reason != "notImplemented" {
		t.Errorf("reason = %q", err.Reason)
	}
}
