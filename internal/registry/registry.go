package registry

import (
	"sync"

	"modeldeck/internal/config"
)

// Registry indexes entries by id, model name/alias, and port. It is safe for
// concurrent use so entries can be added/removed while serving traffic.
type Registry struct {
	mu      sync.RWMutex
	entries []*config.Entry
	byID    map[string]*config.Entry
	byModel map[string]*config.Entry
	byPort  map[int]*config.Entry
}

func New(c *config.Config) *Registry {
	r := &Registry{}
	r.Rebuild(c)
	return r
}

// Rebuild replaces the index from the given config.
func (r *Registry) Rebuild(c *config.Config) {
	byID := make(map[string]*config.Entry, len(c.Entries))
	byModel := make(map[string]*config.Entry, len(c.Entries)*2)
	byPort := make(map[int]*config.Entry, len(c.Entries))
	entries := make([]*config.Entry, 0, len(c.Entries))

	for _, e := range c.Entries {
		entries = append(entries, e)
		byID[e.ID] = e
		byModel[e.ID] = e
		for _, a := range e.Aliases {
			if a != "" {
				byModel[a] = e
			}
		}
		if e.Port > 0 {
			if _, ok := byPort[e.Port]; !ok {
				byPort[e.Port] = e
			}
		}
	}

	r.mu.Lock()
	r.entries, r.byID, r.byModel, r.byPort = entries, byID, byModel, byPort
	r.mu.Unlock()
}

func (r *Registry) ByID(id string) (*config.Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	return e, ok
}

// ByModel resolves a model name or alias to an entry.
func (r *Registry) ByModel(name string) (*config.Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byModel[name]
	return e, ok
}

func (r *Registry) ByPort(port int) (*config.Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byPort[port]
	return e, ok
}

func (r *Registry) Entries() []*config.Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*config.Entry(nil), r.entries...)
}
