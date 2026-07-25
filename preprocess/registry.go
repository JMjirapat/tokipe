package preprocess

import "sync"

// Registry is a small ordered collection of named rules. It exists so an
// application can assemble its rule set from several places (init of a plugin
// package, a config loader, a test) and then build a Stage from it once.
//
// Order is registration order and is preserved: rules registered earlier win.
// Registering a name that already exists replaces the rule in place, keeping
// its original position, so a caller can override a default rule without
// disturbing precedence.
//
// A Registry is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	names []string
	rules map[string]Rule
}

// NewRegistry returns an empty Registry with the given rules registered in
// order.
func NewRegistry(rules ...Rule) *Registry {
	r := &Registry{rules: map[string]Rule{}}
	r.Register(rules...)
	return r
}

// Register adds rules in order. Nil rules and rules with an empty Name are
// ignored. A rule whose name is already registered replaces the existing one
// at its existing position.
func (r *Registry) Register(rules ...Rule) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rules == nil {
		r.rules = map[string]Rule{}
	}
	for _, rule := range rules {
		if rule == nil {
			continue
		}
		name := rule.Name()
		if name == "" {
			continue
		}
		if _, exists := r.rules[name]; !exists {
			r.names = append(r.names, name)
		}
		r.rules[name] = rule
	}
	return r
}

// Remove deletes a rule by name. It reports whether the rule was present.
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rules[name]; !ok {
		return false
	}
	delete(r.rules, name)
	for i, n := range r.names {
		if n == name {
			r.names = append(r.names[:i], r.names[i+1:]...)
			break
		}
	}
	return true
}

// Lookup returns the rule registered under name.
func (r *Registry) Lookup(name string) (Rule, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rule, ok := r.rules[name]
	return rule, ok
}

// Len returns the number of registered rules.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.names)
}

// Names returns the registered rule names in order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.names...)
}

// Rules returns the registered rules in order.
func (r *Registry) Rules() []Rule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Rule, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, r.rules[n])
	}
	return out
}

// Stage builds a Stage from the registry's current contents. Later changes to
// the registry do not affect an already-built Stage.
func (r *Registry) Stage(opts ...Option) *Stage {
	return NewStageWith(r.Rules(), opts...)
}
