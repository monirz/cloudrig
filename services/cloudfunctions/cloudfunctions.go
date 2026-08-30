// Package cloudfunctions serves the Cloud Functions v1 REST API, so real
// gcloud and the GCP SDKs can drive the emulator.
//
// v1 rather than v2 for one reason: v1 is the only generation with an invoke
// method. A v2 function is a Cloud Run service, called at its own URL with an
// identity token, so a v2-only emulator can store function metadata but can
// never run one.
package cloudfunctions

import (
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/functions"
	"github.com/monirz/cloudrig/transport"
)

// Prefixes are the path prefixes this service claims.
//
// v2 is here for a single endpoint: gcloud validates --runtime against
// /v2/{parent}/runtimes even when deploying a gen1 function.
var Prefixes = []string{"/v1/", "/v2/"}

// Service is the v1 API over a function registry. It holds no state of its own
// beyond operations: the registry is the single source of truth, so the API and
// the runner cannot drift apart.
type Service struct {
	reg    *functions.Registry
	clk    clock.Clock
	router *transport.Router
	ops    *operationStore

	mu      sync.Mutex
	execSeq uint64
}

// New wires the API onto a registry.
func New(reg *functions.Registry, clk clock.Clock) *Service {
	s := &Service{reg: reg, clk: clk, router: transport.NewRouter(), ops: newOperationStore()}

	const parent = "/v1/projects/{project}/locations/{location}"
	s.router.Handle(http.MethodGet, parent+"/functions", s.listFunctions)
	s.router.Handle(http.MethodPost, parent+"/functions", s.createFunction)
	s.router.Handle(http.MethodGet, parent+"/functions/{name}", s.getFunction)
	s.router.Handle(http.MethodPost, parent+"/functions/{name}", s.postFunction)
	s.router.Handle(http.MethodPatch, parent+"/functions/{name}", s.patchFunction)
	s.router.Handle(http.MethodDelete, parent+"/functions/{name}", s.deleteFunction)
	s.router.Handle(http.MethodPost, parent+"/functions:generateUploadUrl", s.generateUploadURL)

	s.router.Handle(http.MethodGet, "/v1/projects/{project}/locations", s.listLocations)

	// v2 is read-only: the same descriptors, nested as v2 spells them. gcloud
	// reads through v2 by default, so without this plain `gcloud functions
	// list` fails even though every function is running.
	const v2parent = "/v2/projects/{project}/locations/{location}"
	s.router.Handle(http.MethodGet, v2parent+"/functions", s.listFunctionsV2)
	s.router.Handle(http.MethodGet, v2parent+"/functions/{name}", s.getFunctionV2)
	s.router.Handle(http.MethodGet, "/v1/operations/{operation}", s.getOperation)
	s.router.Handle(http.MethodGet, "/v2/projects/{project}/locations/{location}/runtimes", s.listRuntimes)
	return s
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

func (s *Service) listFunctions(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	descs := s.reg.List(p["project"], wildcard(p["location"]))
	out := make([]v1Function, 0, len(descs))
	for _, d := range descs {
		out = append(out, toV1(d, r))
	}
	return writeJSON(w, http.StatusOK, map[string]any{"functions": out})
}

func (s *Service) listFunctionsV2(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out := make([]v2Function, 0)

	// gcloud lists by calling v1 for gen1 and v2 filtered to GEN_2, then
	// merging. Ignoring the filter makes every function answer both calls and
	// appear twice.
	if wantsEnvironment(r.URL.Query().Get("filter"), environment) {
		for _, d := range s.reg.List(p["project"], wildcard(p["location"])) {
			out = append(out, toV2(d, r))
		}
	}
	return writeJSON(w, http.StatusOK, map[string]any{"functions": out})
}

// wantsEnvironment reports whether a v2 list filter would accept env. Only the
// environment term is understood; any other filter matches everything, which
// errs toward returning too much rather than silently hiding a function.
func wantsEnvironment(filter, env string) bool {
	const key = "environment="
	i := strings.Index(filter, key)
	if i < 0 {
		return true
	}
	want := strings.Trim(filter[i+len(key):], `"`)
	if j := strings.IndexAny(want, ` "`); j >= 0 {
		want = want[:j]
	}
	return want == env
}

func (s *Service) getFunctionV2(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, _ := splitVerb(p["name"])
	desc, ok := s.reg.Get(p["project"], p["location"], name)
	if !ok {
		return s.notFound(p["project"], p["location"], name)
	}
	return writeJSON(w, http.StatusOK, toV2(desc, r))
}

// wildcard maps the "-" GCP uses for "every location" onto the registry's
// "no filter".
func wildcard(location string) string {
	if location == "-" {
		return ""
	}
	return location
}

func (s *Service) getFunction(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, _ := splitVerb(p["name"])
	desc, ok := s.reg.Get(p["project"], p["location"], name)
	if !ok {
		return s.notFound(p["project"], p["location"], name)
	}
	return writeJSON(w, http.StatusOK, toV1(desc, r))
}

// postFunction handles the custom methods, which arrive as a verb suffixed to
// the resource segment: .../functions/hello:call.
func (s *Service) postFunction(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb := splitVerb(p["name"])
	switch verb {
	case "call":
		return s.callFunction(w, r, p, name)
	case "":
		return gerr.Newf(gerr.InvalidArgument, "POST on a function needs a method, e.g. %s:call", name).
			WithHTTPStatus(http.StatusMethodNotAllowed).
			WithReason("methodNotAllowed")
	default:
		return gerr.NewUnimplemented("cloudfunctions.projects.locations.functions." + verb)
	}
}

// createFunction and patchFunction are loud rather than silently wrong: real
// gcloud uploads a source zip, which the emulator cannot yet accept.
func (s *Service) createFunction(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return deployUnimplemented("cloudfunctions.projects.locations.functions.create")
}

func (s *Service) patchFunction(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return deployUnimplemented("cloudfunctions.projects.locations.functions.patch")
}

func (s *Service) generateUploadURL(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	return deployUnimplemented("cloudfunctions.projects.locations.functions.generateUploadUrl")
}

func deployUnimplemented(op string) error {
	err := gerr.NewUnimplemented(op)
	err.Message += "; deploy with: cloudrig fn deploy NAME --source DIR"
	return err
}

func (s *Service) deleteFunction(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, _ := splitVerb(p["name"])
	if err := s.reg.Delete(p["project"], p["location"], name); err != nil {
		return s.notFound(p["project"], p["location"], name)
	}
	op := s.ops.complete(s.clk.Now(), map[string]any{})
	return writeJSON(w, http.StatusOK, op)
}

func (s *Service) getOperation(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	op, ok := s.ops.get(p["operation"])
	if !ok {
		return gerr.Newf(gerr.NotFound, "operation %q not found", p["operation"]).
			WithHTTPStatus(http.StatusNotFound).
			WithReason("notFound")
	}
	return writeJSON(w, http.StatusOK, op)
}

// listLocations answers the region discovery gcloud performs before listing
// functions. It reports the default plus every location actually in use, so a
// function deployed to an unusual region is still discoverable.
func (s *Service) listLocations(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	seen := map[string]bool{functions.DefaultLocation: true}
	for _, d := range s.reg.List(p["project"], "") {
		seen[d.Location] = true
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{
			"name":       "projects/" + p["project"] + "/locations/" + name,
			"locationId": name,
		})
	}
	return writeJSON(w, http.StatusOK, map[string]any{"locations": out})
}

// listRuntimes answers the runtime validation gcloud performs before a deploy.
// The filter is ignored: reporting every runtime we support is a superset of
// any filter gcloud sends, and the client filters again on its side.
func (s *Service) listRuntimes(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var out []map[string]any
	for _, name := range functions.KnownRuntimes() {
		for _, env := range []string{"GEN_1", "GEN_2"} {
			out = append(out, map[string]any{
				"name":        name,
				"displayName": name,
				"stage":       "GA",
				"environment": env,
			})
		}
	}
	return writeJSON(w, http.StatusOK, map[string]any{"runtimes": out})
}

// splitVerb separates a custom method from the resource id: "hello:call".
func splitVerb(segment string) (name, verb string) {
	name, verb, _ = strings.Cut(segment, ":")
	return name, verb
}

// notFound reports a missing function, and says where a function of that name
// actually lives if one does. Deploying to one project and invoking from
// another is the easiest mistake to make, and "does not exist" alone sends you
// looking for the wrong problem.
func (s *Service) notFound(project, location, name string) error {
	msg := "Function " + functions.ResourceName(project, location, name) + " does not exist"

	var elsewhere []string
	for _, d := range s.reg.List("", "") {
		if d.Name == name {
			elsewhere = append(elsewhere, d.ResourceName())
		}
	}
	switch len(elsewhere) {
	case 0:
	case 1:
		msg += "; a function of that name is deployed at " + elsewhere[0]
	default:
		msg += "; functions of that name are deployed at " + strings.Join(elsewhere, ", ")
	}

	return gerr.New(gerr.NotFound, msg).
		WithHTTPStatus(http.StatusNotFound).
		WithReason("notFound")
}
