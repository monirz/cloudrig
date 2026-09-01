package cloudrun

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
)

// Prefixes are the path prefixes this service claims. The Knative one is its
// own; /v1/ is shared with Cloud Functions and Pub/Sub, so the front door asks
// which service has a route rather than splitting on the prefix.
var Prefixes = []string{"/apis/serving.knative.dev/"}

// API serves the Cloud Run Admin API.
//
// Two surfaces, because gcloud uses both: the regional one is Knative's
// serving API under /apis/serving.knative.dev/v1, and the global one is
// /v1/projects/{project}/locations/{location}/services, which is what
// `gcloud run services list` without a --region reaches.
type API struct {
	router *transport.Router
	reg    *Registry
}

// NewAPI wires the routes over a registry.
func NewAPI(reg *Registry) *API {
	a := &API{router: transport.NewRouter(), reg: reg}

	const knative = "/apis/serving.knative.dev/v1/namespaces/{namespace}/services"
	a.router.Handle(http.MethodGet, knative, a.listKnative)
	a.router.Handle(http.MethodPost, knative, a.createKnative)
	a.router.Handle(http.MethodGet, knative+"/{name}", a.getKnative)
	a.router.Handle(http.MethodPut, knative+"/{name}", a.replaceKnative)
	a.router.Handle(http.MethodDelete, knative+"/{name}", a.deleteKnative)

	const revisions = "/apis/serving.knative.dev/v1/namespaces/{namespace}/revisions"
	a.router.Handle(http.MethodGet, revisions, a.listRevisions)
	a.router.Handle(http.MethodGet, revisions+"/{name}", a.getRevision)

	const global = "/v1/projects/{project}/locations/{location}/services"
	a.router.Handle(http.MethodGet, global, a.listGlobal)
	a.router.Handle(http.MethodGet, global+"/{name}", a.getGlobal)
	a.router.Handle(http.MethodGet, global+"/{name}:getIamPolicy", a.getIamPolicy)
	a.router.Handle(http.MethodDelete, global+"/{name}", a.deleteGlobal)

	// The IAM verbs gcloud calls on the way to a deploy. The name segment
	// carries them as {name}:verb, which the router captures whole.
	a.router.Handle(http.MethodPost, global+"/{name}", a.serviceVerb)
	return a
}

// Matches reports whether a route here claims the request, so /v1/ can be
// shared with the other services mounted on it.
func (a *API) Matches(method, escapedPath string) bool {
	return a.router.Matches(method, escapedPath)
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

// regionOf reads the region gcloud puts in a header or a query parameter on
// the Knative surface, where the path carries only the namespace.
func regionOf(r *http.Request) string {
	if region := r.URL.Query().Get("region"); region != "" {
		return region
	}
	if region := r.Header.Get("X-Goog-Request-Params"); strings.Contains(region, "location=") {
		_, after, _ := strings.Cut(region, "location=")
		if before, _, found := strings.Cut(after, "&"); found {
			return before
		}
		return after
	}
	return DefaultLocation
}

func (a *API) createKnative(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var body knativeService
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}

	svc, err := fromKnative(body, p["namespace"], regionOf(r))
	if err != nil {
		return gerr.New(gerr.InvalidArgument, err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if svc.Name == "" {
		return gerr.New(gerr.InvalidArgument, "metadata.name is required").
			WithHTTPStatus(http.StatusBadRequest)
	}
	if _, exists := a.reg.Describe(svc.Project, svc.Location, svc.Name); exists {
		return gerr.New(gerr.AlreadyExists, "service "+svc.Name+" already exists").
			WithHTTPStatus(http.StatusConflict)
	}
	return a.deploy(w, r, svc)
}

func (a *API) replaceKnative(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var body knativeService
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}

	svc, err := fromKnative(body, p["namespace"], regionOf(r))
	if err != nil {
		return gerr.New(gerr.InvalidArgument, err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if svc.Name == "" {
		svc.Name = p["name"]
	}
	return a.deploy(w, r, svc)
}

// deploy runs the service and answers with what it became.
//
// Synchronous, unlike the real API's long-running operation: the container is
// already serving when this returns, so a Ready status is the truth rather
// than a promise. gcloud polls until Ready and finds it immediately.
func (a *API) deploy(w http.ResponseWriter, r *http.Request, svc Service) error {
	deployed, err := a.reg.Deploy(r.Context(), svc, Options{})
	if err != nil {
		return gerr.New(gerr.FailedPrecondition, err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	return writeJSON(w, http.StatusOK, toKnative(deployed, serviceURL(r, deployed)))
}

func (a *API) getKnative(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	svc, ok := a.reg.Describe(p["namespace"], regionOf(r), p["name"])
	if !ok {
		return notFound(p["name"])
	}
	return writeJSON(w, http.StatusOK, toKnative(svc, serviceURL(r, svc)))
}

func (a *API) listKnative(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	services := a.reg.List(p["namespace"], "")

	items := make([]knativeService, 0, len(services))
	for _, svc := range services {
		items = append(items, toKnative(svc, serviceURL(r, svc)))
	}
	return writeJSON(w, http.StatusOK, knativeList{
		APIVersion: "serving.knative.dev/v1",
		Kind:       "ServiceList",
		Items:      items,
	})
}

func (a *API) deleteKnative(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	if err := a.reg.Delete(p["namespace"], regionOf(r), p["name"]); err != nil {
		return notFound(p["name"])
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Success",
	})
}

// listRevisions answers `gcloud run revisions list`.
//
// One revision per service: a deploy replaces what was running rather than
// keeping the old container alive, so there is no history to report. Real
// Cloud Run keeps every revision and can split traffic between them.
func (a *API) listRevisions(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	services := a.reg.List(p["namespace"], "")

	items := make([]knativeRevision, 0, len(services))
	for _, svc := range services {
		items = append(items, toRevision(svc))
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "serving.knative.dev/v1",
		"kind":       "RevisionList",
		"metadata":   listMeta{},
		"items":      items,
	})
}

func (a *API) getRevision(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	for _, svc := range a.reg.List(p["namespace"], "") {
		if svc.Revision() == p["name"] {
			return writeJSON(w, http.StatusOK, toRevision(svc))
		}
	}
	return notFound(p["name"])
}

// The global surface reports the same services with the v2-style resource
// name, which is what `gcloud run services list` renders.
func (a *API) listGlobal(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	location := p["location"]
	if location == "-" {
		location = ""
	}

	// The Knative list shape, not the v2 one: this path is /v1/, which
	// gcloud reads as serving.knative.dev. A v2-shaped reply parses without
	// error and lists nothing, which is how this was found.
	services := a.reg.List(p["project"], location)
	items := make([]knativeService, 0, len(services))
	for _, svc := range services {
		items = append(items, toKnative(svc, serviceURL(r, svc)))
	}
	return writeJSON(w, http.StatusOK, knativeList{
		APIVersion: "serving.knative.dev/v1",
		Kind:       "ServiceList",
		Items:      items,
	})
}

func (a *API) getGlobal(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	svc, ok := a.reg.Describe(p["project"], p["location"], p["name"])
	if !ok {
		return notFound(p["name"])
	}
	return writeJSON(w, http.StatusOK, globalService(svc, serviceURL(r, svc)))
}

// serviceVerb answers the :verb forms on a service resource.
//
// gcloud asks whether it may allow unauthenticated invocations before every
// deploy, and refuses to continue if the question 404s. There is no IAM here
// to consult, so every permission asked for is granted — the emulator has no
// authentication to enforce, which UNSUPPORTED.md says plainly.
func (a *API) serviceVerb(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb, ok := strings.Cut(p["name"], ":")
	if !ok {
		return gerr.New(gerr.InvalidArgument, "POST on a service needs a verb").
			WithHTTPStatus(http.StatusBadRequest)
	}

	switch verb {
	case "testIamPermissions":
		var body struct {
			Permissions []string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		return writeJSON(w, http.StatusOK, map[string]any{"permissions": body.Permissions})

	case "setIamPolicy":
		var body struct {
			Policy map[string]any `json:"policy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Policy == nil {
			body.Policy = map[string]any{}
		}
		body.Policy["etag"] = "cloudrig"
		return writeJSON(w, http.StatusOK, body.Policy)
	}
	return gerr.New(gerr.Unimplemented, "unsupported verb "+verb+" on service "+name).
		WithHTTPStatus(http.StatusNotImplemented)
}

func (a *API) deleteGlobal(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	if err := a.reg.Delete(p["project"], p["location"], p["name"]); err != nil {
		return notFound(p["name"])
	}
	return writeJSON(w, http.StatusOK, map[string]any{"done": true})
}

// globalService is the v1/v2 projects-and-locations shape.
func globalService(svc Service, url string) map[string]any {
	return map[string]any{
		"name":                  svc.ResourceName(),
		"uri":                   url,
		"latestReadyRevision":   svc.ResourceName() + "/revisions/" + svc.Revision(),
		"latestCreatedRevision": svc.ResourceName() + "/revisions/" + svc.Revision(),
		"template": map[string]any{
			"containers": []map[string]any{{"image": svc.Image}},
		},
		"terminalCondition": map[string]any{"type": "Ready", "state": "CONDITION_SUCCEEDED"},
	}
}

// serviceURL is where the emulator serves a deployed service, mirroring how a
// function is addressed.
//
// Built from the request rather than from configuration: the emulator's port
// is often chosen at startup, and a URL gcloud prints has to be one a caller
// can paste.
func serviceURL(r *http.Request, svc Service) string {
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return "http://" + host + "/" + svc.Location + "-" + svc.Project + "/" + svc.Name
}

// getIamPolicy answers with an empty policy: there is nothing to grant, and a
// missing policy stops a deploy that only wanted to read one.
func (a *API) getIamPolicy(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return writeJSON(w, http.StatusOK, map[string]any{"version": 1, "etag": "cloudrig"})
}

func notFound(name string) error {
	return gerr.New(gerr.NotFound, "service "+name+" not found").
		WithHTTPStatus(http.StatusNotFound)
}

func writeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(body)
}
