package services

import (
	"slices"
	"strings"
)

// builtins is every provider this build carries, in catalogue order (the
// order "What SwarmMemo gives agents" lists them in): the most useful first,
// screening and the services a caller can use without a key, then memory.
// Adding a provider is its own file, with its catalogue fields filled in
// (catalog.go), plus one line here; every discovery surface then lists it.
var builtins = []func(Deps) Provider{
	newScreen,
	newInference,
	newPublicData,
	newX402,
	newNotary,
	newMemory,
	newWakeup,
	newReceiver,
	newFetch,
	newCorroborate,
	newPaste,
	newDocs,
	newRuns,
	newEcho,
	newTools, // last: it routes to the others; the discovery surfaces lead with it
}

// Known is the ids of every built-in provider, sorted: the values the
// SERVICES flag may name.
func Known() []string {
	ids := make([]string, 0, len(builtins))
	for _, build := range builtins {
		ids = append(ids, build(Deps{}).Describe().ID)
	}
	slices.Sort(ids)
	return ids
}

func buildSchema() string {
	var b strings.Builder
	b.WriteString(coreSchema)
	for _, build := range builtins {
		if s, ok := build(Deps{}).(Schemer); ok {
			b.WriteString(s.Schema())
		}
	}
	return b.String()
}

// Registry is the enabled providers, by service id.
type Registry struct {
	enabled   map[string]bool
	providers map[string]Provider
	order     []string
}

// NewRegistry enables the named services (the SERVICES flag). A provider that
// is registered but not enabled is not listed and cannot be called. tools,
// the one catalogue and call over the others, is enabled with any service
// but echo: it only routes, so it needs no flag of its own.
func NewRegistry(enabled []string) *Registry {
	r := &Registry{enabled: map[string]bool{}, providers: map[string]Provider{}}
	for _, id := range enabled {
		r.enabled[id] = true
	}
	if ToolsEnabled(enabled) {
		r.enabled[ToolsID] = true
	}
	return r
}

// NewBuiltinRegistry is NewRegistry with every built-in provider registered.
func NewBuiltinRegistry(enabled []string, deps Deps) *Registry {
	r := NewRegistry(enabled)
	for _, build := range builtins {
		r.Register(build(deps))
	}
	return r
}

// Register adds a provider; a second provider with the same id replaces the
// first in place.
func (r *Registry) Register(p Provider) {
	id := p.Describe().ID
	if _, ok := r.providers[id]; !ok {
		r.order = append(r.order, id)
	}
	r.providers[id] = p
}

// Lookup returns an enabled provider.
func (r *Registry) Lookup(id string) (Provider, error) {
	p, ok := r.providers[id]
	if !ok || !r.enabled[id] {
		return nil, UnknownService(id, r.Enabled())
	}
	return p, nil
}

// List is the catalogue of enabled providers, in registration order.
func (r *Registry) List() []Descriptor {
	var out []Descriptor
	for _, id := range r.order {
		if r.enabled[id] {
			out = append(out, r.providers[id].Describe())
		}
	}
	return out
}

// Enabled is the ids of the enabled, registered providers, in registration order.
func (r *Registry) Enabled() []string {
	var out []string
	for _, id := range r.order {
		if r.enabled[id] {
			out = append(out, id)
		}
	}
	return out
}

func (d Descriptor) method(name string) (Method, bool) {
	for _, m := range d.Methods {
		if m.Name == name {
			return m, true
		}
	}
	return Method{}, false
}
