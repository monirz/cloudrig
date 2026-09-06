// Package serviceusage tracks which APIs a project has enabled.
//
// It is deploy-tooling plumbing, not something an app calls at runtime: a
// `terraform apply` or a `gcloud services enable` turns an API on before using
// it, and a stub that always answers "enabled" makes a config that toggles a
// service look like it changed nothing. Tracking real state lets that config
// round-trip — enable, read back enabled, disable, read back disabled.
package serviceusage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/transport"
)

// MaxBodyBytes caps a JSON request body; the port is shared.
const MaxBodyBytes = 4 << 20

// Service is the Service Usage REST API over the shared store.
type Service struct {
	router *transport.Router
	kv     store.Store

	// mu serialises a state change: the store has no unconditional Put, so a
	// write is read-version-then-put, and two overlapping enable/disable calls
	// would otherwise interleave and lose one.
	mu sync.Mutex
}

// New wires the routes gcloud and Terraform drive.
func New(kv store.Store) *Service {
	a := &Service{router: transport.NewRouter(), kv: kv}

	const services = "/v1/projects/{project}/services"
	a.router.Handle(http.MethodGet, services, a.list)
	a.router.Handle(http.MethodGet, services+"/{service}", a.get)
	a.router.Handle(http.MethodPost, services+"/{service}", a.serviceVerb) // :enable / :disable
	a.router.Handle(http.MethodPost, services+":batchEnable", a.batchEnable)
	return a
}

// Reset clears the enablement state for a project, or all of it when project
// is empty. The emulator's Reset calls this, since this state lives under its
// own key prefix rather than the storage tree a project reset otherwise clears.
func (a *Service) Reset(ctx context.Context, project string) error {
	prefix := "su/"
	if project != "" {
		prefix += project + "/"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kv.Reset(ctx, prefix)
}

// Matches reports whether a route here claims the request; /v1/ is shared.
func (a *Service) Matches(method, escapedPath string) bool {
	return a.router.Matches(method, escapedPath)
}

func (a *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

// stateKey records whether one service is enabled for a project.
func stateKey(project, service string) string {
	return "su/" + project + "/" + service
}

// enabled reports a service's state. The default is enabled: an emulator has no
// billing or quota to gate a service behind, so a service nobody has touched is
// treated as on — only an explicit disable turns it off.
func (a *Service) enabled(ctx context.Context, project, service string) bool {
	raw, _, err := a.kv.Get(ctx, stateKey(project, service))
	if err != nil {
		return true
	}
	return string(raw) != "DISABLED"
}

func (a *Service) setState(ctx context.Context, project, service string, on bool) error {
	val := []byte("ENABLED")
	if !on {
		val = []byte("DISABLED")
	}
	key := stateKey(project, service)

	// Under the lock, write at the version we just read: last write wins, but
	// no two calls interleave, and a failed write is reported rather than
	// answered with a false success.
	a.mu.Lock()
	defer a.mu.Unlock()
	_, version, err := a.kv.Get(ctx, key)
	if err != nil {
		version = 0 // absent
	}
	if _, err := a.kv.Put(ctx, key, val, version); err != nil {
		return err
	}
	return nil
}

func (a *Service) serviceState(project, service string, ctx context.Context) map[string]any {
	state := "DISABLED"
	if a.enabled(ctx, project, service) {
		state = "ENABLED"
	}
	return map[string]any{
		"name":   "projects/" + project + "/services/" + service,
		"config": map[string]any{"name": service},
		"state":  state,
		"parent": "projects/" + project,
	}
}

func (a *Service) get(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return writeJSON(w, http.StatusOK, a.serviceState(p["project"], p["service"], r.Context()))
}

func (a *Service) list(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	entries, _, err := a.kv.List(r.Context(), "su/"+p["project"]+"/", 0, "")
	if err != nil {
		return gerr.New(gerr.Internal, "listing services: "+err.Error())
	}

	filter := r.URL.Query().Get("filter")
	out := make([]map[string]any, 0, len(entries))
	for _, kv := range entries {
		service := kv.Key[strings.LastIndex(kv.Key, "/")+1:]
		on := string(kv.Val) != "DISABLED"
		switch filter {
		case "state:ENABLED":
			if !on {
				continue
			}
		case "state:DISABLED":
			if on {
				continue
			}
		}
		out = append(out, a.serviceState(p["project"], service, r.Context()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// serviceVerb answers :enable and :disable, which ride on the name segment.
func (a *Service) serviceVerb(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	service, verb, ok := strings.Cut(p["service"], ":")
	if !ok {
		return gerr.New(gerr.InvalidArgument, "a POST on a service needs a verb").
			WithHTTPStatus(http.StatusBadRequest)
	}
	switch verb {
	case "enable":
		_ = a.setState(r.Context(), p["project"], service, true)
	case "disable":
		_ = a.setState(r.Context(), p["project"], service, false)
	default:
		return gerr.NewUnimplemented("serviceusage.services." + verb)
	}
	// A long-running operation, reported already done: the change is durable
	// before the response, so DONE is the truth.
	return writeJSON(w, http.StatusOK, map[string]any{
		"name": "operations/enable_" + service,
		"done": true,
		"response": map[string]any{
			"@type":   "type.googleapis.com/google.api.serviceusage.v1.EnableServiceResponse",
			"service": a.serviceState(p["project"], service, r.Context()),
		},
	})
}

// batchEnable turns several services on at once, which is how Terraform enables
// a set.
func (a *Service) batchEnable(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var body struct {
		ServiceIds []string `json:"serviceIds"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	for _, id := range body.ServiceIds {
		_ = a.setState(r.Context(), p["project"], id, true)
	}
	return writeJSON(w, http.StatusOK, map[string]any{"name": "operations/batchEnable", "done": true})
}

func decode(r *http.Request, into any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return gerr.New(gerr.InvalidArgument, "reading the request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if len(body) > MaxBodyBytes {
		return gerr.New(gerr.InvalidArgument, "the request body is too large").
			WithHTTPStatus(http.StatusRequestEntityTooLarge)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed JSON body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(body)
}
