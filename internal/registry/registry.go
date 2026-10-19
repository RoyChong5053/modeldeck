package registry

import "modeldeck/internal/config"

// Registry indexes entries by id, model name/alias, and port.
type Registry struct {
	entries []*config.Entry
	byID    map[string]*config.Entry
	byModel map[string]*config.Entry
	byPort  map[int]*config.Entry
}

func New(c *config.Config) *Registry {
	r := &Registry{
		byID:    map[string]*config.Entry{},
		byModel: map[string]*config.Entry{},
		byPort:  map[int]*config.Entry{},
	}
	r.entries = append(r.entries, c.Entries...)
	for _, e := range c.Entries {
		r.byID[e.ID] = e
		r.byModel[e.ID] = e
		for _, a := range e.Aliases {
			if a != "" {
				r.byModel[a] = e
			}
		}
		if e.Port > 0 {
			if _, ok := r.byPort[e.Port]; !ok {
				r.byPort[e.Port] = e
			}
		}
	}
	return r
}

func (r *Registry) ByID(id string) (*config.Entry, bool) {
	e, ok := r.byID[id]
	return e, ok
}

// ByModel resolves a model name or alias to an entry.
func (r *Registry) ByModel(name string) (*config.Entry, bool) {
	e, ok := r.byModel[name]
	return e, ok
}

func (r *Registry) ByPort(port int) (*config.Entry, bool) {
	e, ok := r.byPort[port]
	return e, ok
}

func (r *Registry) Entries() []*config.Entry {
	return r.entries
}
