package cloudfunctions

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
)

// callRequest is v1's CallFunctionRequest. Data is a string, not an object:
// callers embed JSON inside it, which is why gcloud --data arrives escaped.
type callRequest struct {
	Data string `json:"data"`
}

// callResponse is v1's CallFunctionResponse. A function that returns an error
// status fills Error rather than failing the API call, matching real GCF: the
// invocation succeeded, the function did not.
type callResponse struct {
	ExecutionID string `json:"executionId"`
	Result      string `json:"result,omitempty"`
	Error       string `json:"error,omitempty"`
}

// callFunction runs a deployed function and returns what it wrote.
//
// The call is made in process against the registry's handler rather than over
// the loopback interface: it is the same handler an HTTP request would reach,
// with one less hop to misreport.
func (s *Service) callFunction(w http.ResponseWriter, r *http.Request, p transport.Params, name string) error {
	handler, ok := s.reg.Handler(p["project"], p["location"], name)
	if !ok {
		return notFound(p["project"], p["location"], name)
	}

	var body callRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			return gerr.Wrap(err, gerr.InvalidArgument, "malformed CallFunctionRequest").
				WithHTTPStatus(http.StatusBadRequest).
				WithReason("parseError")
		}
	}

	sub, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/", strings.NewReader(body.Data))
	if err != nil {
		return err
	}
	sub.Header.Set("Content-Type", "application/json")
	sub.ContentLength = int64(len(body.Data))

	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	handler.ServeHTTP(rec, sub)

	out := callResponse{ExecutionID: s.executionID()}
	if rec.status >= http.StatusBadRequest {
		out.Error = rec.body.String()
	} else {
		out.Result = rec.body.String()
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Service) executionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execSeq++
	return strconv.FormatInt(s.clk.Now().UnixNano(), 36) + "-" + strconv.FormatUint(s.execSeq, 36)
}

// recorder captures a handler's response. net/http/httptest would do, but it
// registers an httptest.serve flag on import, which has no business in a
// shipped binary.
type recorder struct {
	header  http.Header
	body    bytes.Buffer
	status  int
	written bool
}

func (rec *recorder) Header() http.Header { return rec.header }

func (rec *recorder) Write(p []byte) (int, error) {
	rec.written = true
	return rec.body.Write(p)
}

func (rec *recorder) WriteHeader(status int) {
	if !rec.written {
		rec.status = status
		rec.written = true
	}
}
