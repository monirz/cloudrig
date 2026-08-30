package cloudfunctions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/functions"
	"github.com/monirz/cloudrig/transport"
)

// deployRequest is the slice of the v1 CloudFunction body a deploy needs.
type deployRequest struct {
	Name            string `json:"name"`
	Runtime         string `json:"runtime"`
	EntryPoint      string `json:"entryPoint"`
	SourceUploadURL string `json:"sourceUploadUrl"`
}

// deployFunction handles both create and patch: gcloud sends a full function
// body either way, and the emulator has no partial-update semantics worth
// preserving — a deploy replaces what was running.
func (s *Service) deployFunction(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	if s.uploads == nil || s.sources == "" {
		return gerr.New(gerr.Internal, "no temporary directory for uploaded sources").
			WithHTTPStatus(http.StatusInternalServerError)
	}

	var body deployRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return gerr.Wrap(err, gerr.InvalidArgument, "malformed CloudFunction body").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("parseError")
	}

	name, _ := splitVerb(p["name"])
	if name == "" {
		name = lastSegment(body.Name)
	}
	if name == "" {
		return gerr.New(gerr.InvalidArgument, "the function body has no name").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("required")
	}

	source, err := s.materialize(body.SourceUploadURL, name)
	if err != nil {
		return err
	}

	desc, err := s.reg.Deploy(r.Context(), functions.Function{
		Project:    p["project"],
		Location:   p["location"],
		Name:       name,
		Source:     source,
		Runtime:    functions.Runtime(body.Runtime),
		EntryPoint: body.EntryPoint,
	})
	if err != nil {
		// A source that does not build is the caller's problem, so report it
		// with the build output rather than as an emulator fault.
		return gerr.Wrap(err, gerr.InvalidArgument, "%s", err.Error()).
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("deployFailed")
	}

	op := s.ops.complete(s.clk.Now(), map[string]any{
		"@type": "type.googleapis.com/google.cloud.functions.v1.CloudFunction",
		"name":  desc.ResourceName(),
	})
	return writeJSON(w, http.StatusOK, op)
}

// materialize turns an upload URL into a directory the runner can build.
func (s *Service) materialize(uploadURL, name string) (string, error) {
	token := lastSegment(uploadURL)
	archive, ok := s.uploads.path(token)
	if token == "" || !ok {
		return "", gerr.New(gerr.InvalidArgument,
			"sourceUploadUrl is not an upload issued by this emulator; call functions:generateUploadUrl first").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}

	// A directory per upload, so a redeploy never extracts over a tree the
	// previous version is still serving from.
	dir := filepath.Join(s.sources, name+"-"+token)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", gerr.Wrap(err, gerr.Internal, "creating a source directory")
	}
	if err := extract(archive, dir); err != nil {
		return "", gerr.Wrap(err, gerr.InvalidArgument, "%s", err.Error()).
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}
	if err := installDeps(dir, s.logf); err != nil {
		return "", gerr.Wrap(err, gerr.InvalidArgument, "%s", err.Error()).
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("deployFailed")
	}
	return dir, nil
}

func (s *Service) logf(format string, args ...any) {
	if s.events == nil {
		return
	}
	fmt.Fprintf(s.events, format+"\n", args...)
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
