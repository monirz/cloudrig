package cloudrun

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Registry holds the running services. It is the single source of truth: the
// API is a view over this, never a second store, so a deploy that the API
// reports cannot be one that nothing is serving.
type Registry struct {
	mu         sync.RWMutex
	services   map[string]*Instance
	deployed   map[string]Service
	generation map[string]int
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		services:   map[string]*Instance{},
		deployed:   map[string]Service{},
		generation: map[string]int{},
	}
}

// Deploy builds and starts a service, replacing any earlier revision.
func (r *Registry) Deploy(ctx context.Context, svc Service, o Options) (Service, error) {
	svc, err := svc.resolve()
	if err != nil {
		return Service{}, err
	}
	o.Env = append(append([]string{}, svc.Env...), o.Env...)

	k := key(svc.Project, svc.Location, svc.Name)

	r.mu.Lock()
	svc.Generation = r.generation[k] + 1
	r.mu.Unlock()

	// Started before the old one is retired: a deploy that fails to build
	// leaves the previous revision serving, as a real one does.
	inst, err := start(ctx, svc, svc.Source, o)
	if err != nil {
		return Service{}, err
	}

	r.mu.Lock()
	previous := r.services[k]
	r.services[k] = inst
	r.deployed[k] = svc
	r.generation[k] = svc.Generation
	r.mu.Unlock()

	if previous != nil {
		_ = previous.Stop()
	}
	return svc, nil
}

// Instance returns a running service.
func (r *Registry) Instance(project, location, name string) (*Instance, bool) {
	if project == "" {
		project = DefaultProject
	}
	if location == "" {
		location = DefaultLocation
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.services[key(project, location, name)]
	return inst, ok
}

// Describe returns what was deployed.
func (r *Registry) Describe(project, location, name string) (Service, bool) {
	if project == "" {
		project = DefaultProject
	}
	if location == "" {
		location = DefaultLocation
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	svc, ok := r.deployed[key(project, location, name)]
	return svc, ok
}

// List returns every deployed service, in name order.
func (r *Registry) List(project, location string) []Service {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Service, 0, len(r.deployed))
	for _, svc := range r.deployed {
		if project != "" && svc.Project != project {
			continue
		}
		if location != "" && location != "-" && svc.Location != location {
			continue
		}
		out = append(out, svc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Delete stops a service and forgets it.
func (r *Registry) Delete(project, location, name string) error {
	if project == "" {
		project = DefaultProject
	}
	if location == "" {
		location = DefaultLocation
	}
	k := key(project, location, name)

	r.mu.Lock()
	inst, ok := r.services[k]
	delete(r.services, k)
	delete(r.deployed, k)
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("service %s not found", name)
	}
	return inst.Stop()
}

// StopAll shuts every service down.
func (r *Registry) StopAll() {
	r.mu.Lock()
	running := make([]*Instance, 0, len(r.services))
	for _, inst := range r.services {
		running = append(running, inst)
	}
	r.services = map[string]*Instance{}
	r.deployed = map[string]Service{}
	r.mu.Unlock()

	for _, inst := range running {
		_ = inst.Stop()
	}
}

// Route resolves a request path to a service. The emulator addresses one as
// /{location}-{project}/{service}/..., mirroring the functions layout.
//
// The prefix is matched against what is deployed rather than parsed: both a
// location and a project contain hyphens, so "us-central1-cloudrig-local"
// cannot be split by looking at it. Comparing against known services has no
// such ambiguity, and there are never many.
func (r *Registry) Route(escapedPath string) (http.Handler, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for k, svc := range r.deployed {
		prefix := "/" + svc.Location + "-" + svc.Project + "/" + svc.Name
		if !strings.HasPrefix(escapedPath, prefix) {
			continue
		}

		// The next character must end the segment, or "hello" would claim a
		// request meant for "hello-world".
		rest := escapedPath[len(prefix):]
		if rest != "" && !strings.HasPrefix(rest, "/") {
			continue
		}
		if rest == "" {
			rest = "/"
		}
		if inst, ok := r.services[k]; ok {
			return inst, rest, true
		}
	}
	return nil, "", false
}
