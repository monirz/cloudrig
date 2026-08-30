package functions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
)

// AdminPath is where the registry's admin API is mounted.
const AdminPath = "/_emu/functions"

// DeployRequest is the body of a deploy. Source is a path on the machine
// running the emulator, not an uploaded archive: the emulator and the CLI share
// a filesystem, and pretending otherwise would buy nothing.
type DeployRequest struct {
	Project    string  `json:"project,omitempty"`
	Location   string  `json:"location,omitempty"`
	Name       string  `json:"name"`
	Source     string  `json:"source"`
	Runtime    Runtime `json:"runtime,omitempty"`
	EntryPoint string  `json:"entryPoint,omitempty"`
	Watch      bool    `json:"watch,omitempty"`
}

// Admin serves deploy, list, describe and delete.
func (r *Registry) Admin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(req.URL.EscapedPath(), AdminPath), "/")
		name, sub, _ := strings.Cut(rest, "/")
		project := req.URL.Query().Get("project")
		location := req.URL.Query().Get("location")

		var err error
		switch {
		case name != "" && sub == "logs" && req.Method == http.MethodGet:
			err = r.handleLogs(w, req, project, location, name)
		case sub != "":
			err = gerr.Newf(gerr.NotFound, "no such sub-resource %q", sub).
				WithHTTPStatus(http.StatusNotFound).
				WithReason("notFound")
		case name == "" && req.Method == http.MethodPost:
			err = r.handleDeploy(w, req)
		case name == "" && req.Method == http.MethodGet:
			err = writeJSON(w, http.StatusOK, map[string]any{"functions": r.List(project, location)})
		case name != "" && req.Method == http.MethodGet:
			err = r.handleGet(w, project, location, name)
		case name != "" && req.Method == http.MethodDelete:
			err = r.handleDelete(w, project, location, name)
		default:
			err = gerr.Newf(gerr.InvalidArgument, "method %s not allowed on %s", req.Method, req.URL.EscapedPath()).
				WithHTTPStatus(http.StatusMethodNotAllowed).
				WithReason("methodNotAllowed")
		}
		if err != nil {
			gerr.WriteJSON(w, err)
		}
	})
}

func (r *Registry) handleDeploy(w http.ResponseWriter, req *http.Request) error {
	var body DeployRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return gerr.Wrap(err, gerr.InvalidArgument, "malformed deploy body").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("parseError")
	}

	desc, err := r.Deploy(req.Context(), Function{
		Project:    body.Project,
		Location:   body.Location,
		Name:       body.Name,
		Source:     body.Source,
		Runtime:    body.Runtime,
		EntryPoint: body.EntryPoint,
		Watch:      body.Watch,
	})
	if err != nil {
		// A deploy that fails to build is the caller's problem, not ours, so
		// it is 400 with the compiler output rather than a 500.
		return gerr.Wrap(err, gerr.InvalidArgument, "%s", err.Error()).
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("deployFailed")
	}
	return writeJSON(w, http.StatusOK, desc)
}

func (r *Registry) handleGet(w http.ResponseWriter, project, location, name string) error {
	desc, ok := r.Get(project, location, name)
	if !ok {
		return notDeployed(name)
	}
	return writeJSON(w, http.StatusOK, desc)
}

func (r *Registry) handleDelete(w http.ResponseWriter, project, location, name string) error {
	if err := r.Delete(project, location, name); err != nil {
		return notDeployed(name)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// handleLogs writes a function's recent output, then streams new lines while
// follow is set. Streaming is line-at-a-time with an explicit flush, so a
// follower sees output as the function produces it rather than when a buffer
// happens to fill.
func (r *Registry) handleLogs(w http.ResponseWriter, req *http.Request, project, location, name string) error {
	inst, ok := r.Instance(project, location, name)
	if !ok {
		return notDeployed(name)
	}

	flusher, canStream := w.(http.Flusher)
	follow := req.URL.Query().Get("follow") == "true" && canStream

	// Subscribe before writing the snapshot, so a line produced in between is
	// streamed rather than lost.
	var lines <-chan string
	var stop func()
	if follow {
		lines, stop = inst.FollowLogs()
		defer stop()
	}

	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	for _, line := range inst.LogSnapshot() {
		fmt.Fprintln(w, line)
	}
	if !follow {
		return nil
	}
	flusher.Flush()

	for {
		select {
		case line, open := <-lines:
			if !open {
				return nil
			}
			fmt.Fprintln(w, line)
			flusher.Flush()
		case <-req.Context().Done():
			return nil
		}
	}
}

func notDeployed(name string) error {
	return gerr.Newf(gerr.NotFound, "no function %q is deployed", name).
		WithHTTPStatus(http.StatusNotFound).
		WithReason("notFound")
}

func writeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(body)
}

// compile-time check that a Registry can host functions for the transport.
var _ interface {
	Route(string) (http.Handler, string, bool)
	Admin() http.Handler
} = (*Registry)(nil)
