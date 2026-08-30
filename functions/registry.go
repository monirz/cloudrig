package functions

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/monirz/cloudrig/core/clock"
)

// Descriptor is a deployed function.
//
// It is deliberately neutral: neither the v1 nor the v2 wire shape, but the
// facts both are projections of. Storing a wire shape would make the second API
// a translation of the first rather than a second view of the truth.
type Descriptor struct {
	Project    string    `json:"project"`
	Location   string    `json:"location"`
	Name       string    `json:"name"`
	Source     string    `json:"source"`
	Runtime    Runtime   `json:"runtime"`
	EntryPoint string    `json:"entryPoint"`
	State      string    `json:"state"`
	UpdateTime time.Time `json:"updateTime"`
}

// ResourceName is projects/P/locations/L/functions/F.
func (d Descriptor) ResourceName() string {
	return ResourceName(d.Project, d.Location, d.Name)
}

// Registry holds the functions a running emulator is serving. Deploying a name
// that already exists replaces it, which is also how redeploy and hot reload
// work.
type Registry struct {
	clk  clock.Clock
	opts Options

	mu      sync.RWMutex
	entries map[string]*entry
}

type entry struct {
	inst *Instance
	desc Descriptor
}

// NewRegistry returns an empty registry. opts apply to every function it runs.
func NewRegistry(clk clock.Clock, opts Options) *Registry {
	return &Registry{clk: clk, opts: opts, entries: map[string]*entry{}}
}

// Deploy builds and starts f, replacing any function of the same name.
//
// The replacement is started before the old one is stopped, so a failed deploy
// leaves the previous version serving rather than taking the name down.
func (r *Registry) Deploy(ctx context.Context, f Function) (Descriptor, error) {
	inst, err := Start(ctx, f, r.opts)
	if err != nil {
		return Descriptor{}, err
	}

	desc := Descriptor{
		Project:    inst.Project(),
		Location:   inst.Location(),
		Name:       f.Name,
		Source:     f.Source,
		Runtime:    inst.Runtime(),
		EntryPoint: inst.EntryPoint(),
		State:      "ACTIVE",
		UpdateTime: r.clk.Now(),
	}

	key := desc.ResourceName()

	r.mu.Lock()
	old := r.entries[key]
	r.entries[key] = &entry{inst: inst, desc: desc}
	r.mu.Unlock()

	if old != nil {
		_ = old.inst.Stop()
	}
	return desc, nil
}

// lookup returns an entry by resource name.
func (r *Registry) lookup(resource string) (*entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[resource]
	return e, ok
}

// Get returns one descriptor by project, location and name. Empty project or
// location mean the defaults.
func (r *Registry) Get(project, location, name string) (Descriptor, bool) {
	e, ok := r.lookup(ResourceName(project, location, name))
	if !ok {
		return Descriptor{}, false
	}
	return e.desc, true
}

// GetByResource returns one descriptor by full resource name.
func (r *Registry) GetByResource(resource string) (Descriptor, bool) {
	e, ok := r.lookup(resource)
	if !ok {
		return Descriptor{}, false
	}
	return e.desc, true
}

// Handler returns the handler serving a function, for callers that invoke it
// directly rather than through a URL.
func (r *Registry) Handler(project, location, name string) (http.Handler, bool) {
	e, ok := r.lookup(ResourceName(project, location, name))
	if !ok {
		return nil, false
	}
	return e.inst, true
}

// List returns every descriptor, by resource name. An empty project or
// location matches everything.
func (r *Registry) List(project, location string) []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Descriptor, 0, len(r.entries))
	for _, e := range r.entries {
		if project != "" && e.desc.Project != project {
			continue
		}
		if location != "" && e.desc.Location != location {
			continue
		}
		out = append(out, e.desc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceName() < out[j].ResourceName() })
	return out
}

// Delete stops and removes a function.
func (r *Registry) Delete(project, location, name string) error {
	key := ResourceName(project, location, name)

	r.mu.Lock()
	e, ok := r.entries[key]
	delete(r.entries, key)
	r.mu.Unlock()

	if !ok {
		return fmt.Errorf("no function %q is deployed", name)
	}
	return e.inst.Stop()
}

// StopAll shuts every function down. It is safe to call twice.
func (r *Registry) StopAll() {
	r.mu.Lock()
	entries := r.entries
	r.entries = map[string]*entry{}
	r.mu.Unlock()

	for _, e := range entries {
		_ = e.inst.Stop()
	}
}
