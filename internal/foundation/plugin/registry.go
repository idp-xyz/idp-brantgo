package plugin

import (
	"fmt"
	"sync"
)

// Registration is one plugin compiled into the process.
type Registration struct {
	Manifest   PluginManifest
	Cost       CostProvider
	Quote      QuoteValidator
	Packing    PackingValidator
	Inspection InspectionValidator
}

// Registry is the process-local plugin table. It rejects a duplicate plugin, a duplicate check
// identifier, and a second CostProvider for one document profile.
type Registry struct {
	mu      sync.Mutex
	plugins map[string]Registration
	checks  map[string]string
	costBy  map[DocumentProfile]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{plugins: map[string]Registration{}, checks: map[string]string{}, costBy: map[DocumentProfile]string{}}
}

// Register adds reg. The manifest must validate.
func (r *Registry) Register(reg Registration) error {
	if err := reg.Manifest.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := reg.Manifest.ID
	if _, ok := r.plugins[id]; ok {
		return fmt.Errorf("plugin: %s is already registered", id)
	}
	for _, check := range reg.Manifest.Checks {
		if owner, ok := r.checks[check]; ok {
			return fmt.Errorf("plugin: check %s is already registered by %s", check, owner)
		}
	}
	if reg.Cost != nil {
		profile := reg.Cost.DocumentProfile()
		if reg.Cost.PluginID() != id {
			return fmt.Errorf("plugin: cost provider %s is not %s", reg.Cost.PluginID(), id)
		}
		if _, ok := r.costBy[profile]; ok {
			return CostConflict()
		}
	}
	for _, check := range reg.Manifest.Checks {
		r.checks[check] = id
	}
	if reg.Cost != nil {
		r.costBy[reg.Cost.DocumentProfile()] = id
	}
	r.plugins[id] = reg
	return nil
}

// Get returns the registration of id.
func (r *Registry) Get(id string) (Registration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.plugins[id]
	return reg, ok
}

// All returns the registrations in an unspecified order.
func (r *Registry) All() []Registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Registration, 0, len(r.plugins))
	for _, reg := range r.plugins {
		out = append(out, reg)
	}
	return out
}
