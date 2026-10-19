package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version int      `yaml:"version"`
	Server  Server   `yaml:"server"`
	Budgets Budgets  `yaml:"budgets"`
	Entries []*Entry `yaml:"entries"`

	// Path is the file this config was loaded from (not serialized).
	Path string `yaml:"-"`
}

type Server struct {
	Listen     string `yaml:"listen"`
	Token      string `yaml:"token"`
	StateDir   string `yaml:"state_dir"`
	ScriptsDir string `yaml:"scripts_dir"`
}

type Budgets struct {
	VRAMReserveMB int    `yaml:"vram_reserve_mb"`
	RAMReserveMB  int    `yaml:"ram_reserve_mb"`
	MaxStarting   int    `yaml:"max_starting"`
	OnNoFit       string `yaml:"on_no_fit"`
	QueueTimeoutS int    `yaml:"queue_timeout"`
	TTLDefaultS   int    `yaml:"ttl_default"`

	// SerializeUnknownGPU keeps two GPU entries whose memory has not been
	// measured from starting together (safety default: true).
	SerializeUnknownGPU *bool `yaml:"serialize_unknown_gpu,omitempty"`
}

// SerializeUnknownGPUOn reports the effective value (default true).
func (b Budgets) SerializeUnknownGPUOn() bool {
	return b.SerializeUnknownGPU == nil || *b.SerializeUnknownGPU
}

type Ready struct {
	Type    string `yaml:"type"` // http | tcp | log | sleep | none
	Path    string `yaml:"path"`
	Pattern string `yaml:"pattern"`
	Seconds int    `yaml:"seconds"`
	Timeout int    `yaml:"timeout"` // seconds, total readiness budget
}

type Resources struct {
	VRAMMB int `yaml:"vram_mb"`
	RAMMB  int `yaml:"ram_mb"`
}

type Stop struct {
	GraceS int    `yaml:"grace_s"`
	Hook   string `yaml:"hook"`
}

type Entry struct {
	ID      string   `yaml:"id"`
	Name    string   `yaml:"name,omitempty"`
	Aliases []string `yaml:"aliases,omitempty"`
	Script  string   `yaml:"script"`
	Args    []string `yaml:"args,omitempty"`
	Env     []string `yaml:"env,omitempty"`
	Cwd     string   `yaml:"cwd,omitempty"`
	Mode    string   `yaml:"mode,omitempty"` // server | job (job reserved for P3)
	Host    string   `yaml:"host,omitempty"`
	Port    int      `yaml:"port,omitempty"`
	// UpstreamModel rewrites the request's `model` field before proxying.
	// Useful when a client (DeskSpirit, one-api, ...) uses a friendly name
	// that the model server itself does not accept.
	UpstreamModel string    `yaml:"upstream_model,omitempty"`
	Ready         Ready     `yaml:"ready,omitempty"`
	TTL           *int      `yaml:"ttl,omitempty"`
	Placement     string    `yaml:"placement,omitempty"`
	Resources     Resources `yaml:"resources,omitempty"`
	ManualRes     bool      `yaml:"manual_resources,omitempty"`
	Exclusive     bool      `yaml:"exclusive,omitempty"`
	Evictable     *bool     `yaml:"evictable,omitempty"`
	Pinned        bool      `yaml:"pinned,omitempty"`
	Priority      int       `yaml:"priority,omitempty"`
	TogetherWith  []string  `yaml:"together_with,omitempty"`
	MutexWith     []string  `yaml:"mutex_with,omitempty"`
	Tags          []string  `yaml:"tags,omitempty"`
	Stop          Stop      `yaml:"stop,omitempty"`
}

// DisplayName returns Name, falling back to ID.
func (e *Entry) DisplayName() string {
	if e.Name != "" {
		return e.Name
	}
	return e.ID
}

func (e *Entry) HostOr() string {
	if e.Host == "" {
		return "127.0.0.1"
	}
	return e.Host
}

func (e *Entry) BaseURL() string {
	return fmt.Sprintf("http://%s:%d", e.HostOr(), e.Port)
}

// EvictableOK reports whether the entry may be evicted (default true).
func (e *Entry) EvictableOK() bool {
	return e.Evictable == nil || *e.Evictable
}

// TTLSeconds resolves the effective idle TTL. 0 means "never unload".
func (e *Entry) TTLSeconds(def int) int {
	if e.TTL == nil {
		return def
	}
	return *e.TTL
}

func (e *Entry) StopGrace() int {
	if e.Stop.GraceS <= 0 {
		return 10
	}
	return e.Stop.GraceS
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Path = path
	c.Normalize()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) Normalize() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:12688"
	}
	if c.Server.StateDir == "" {
		home, _ := os.UserHomeDir()
		c.Server.StateDir = filepath.Join(home, ".local", "state", "modeldeck")
	}
	c.Server.StateDir = expandHome(c.Server.StateDir)
	if c.Budgets.OnNoFit == "" {
		c.Budgets.OnNoFit = "queue"
	}
	if c.Budgets.QueueTimeoutS == 0 {
		c.Budgets.QueueTimeoutS = 120
	}
	if c.Budgets.MaxStarting == 0 {
		c.Budgets.MaxStarting = 2
	}
	if c.Budgets.TTLDefaultS == 0 {
		c.Budgets.TTLDefaultS = 300
	}
	for _, e := range c.Entries {
		if e.Mode == "" {
			e.Mode = "server"
		}
		if e.Host == "" {
			e.Host = "127.0.0.1"
		}
		if e.Placement == "" {
			e.Placement = "gpu"
		}
		if e.Ready.Type == "" {
			if e.Port > 0 {
				e.Ready.Type = "tcp"
			} else {
				e.Ready.Type = "none"
			}
		}
		if e.Ready.Timeout <= 0 {
			e.Ready.Timeout = 300
		}
		if e.Ready.Path == "" {
			e.Ready.Path = "/health"
		}
		if e.Script != "" {
			e.Script = expandHome(e.Script)
		}
		if e.Cwd != "" {
			e.Cwd = expandHome(e.Cwd)
		}
	}
}

func (c *Config) Validate() error {
	seenID := map[string]bool{}
	seenModel := map[string]string{}
	seenPort := map[int]string{}
	for _, e := range c.Entries {
		if e.ID == "" {
			return fmt.Errorf("entry with empty id")
		}
		if seenID[e.ID] {
			return fmt.Errorf("duplicate entry id %q", e.ID)
		}
		seenID[e.ID] = true
		switch e.Mode {
		case "server", "job":
		default:
			return fmt.Errorf("entry %q: invalid mode %q", e.ID, e.Mode)
		}
		if e.Mode == "server" {
			if e.Port < 1 || e.Port > 65535 {
				return fmt.Errorf("entry %q: invalid port %d", e.ID, e.Port)
			}
			if prev, ok := seenPort[e.Port]; ok {
				// Same port across entries is legal (they are mutually
				// exclusive by lane), but warn via no error. Keep a note.
				_ = prev
			}
			seenPort[e.Port] = e.ID
		}
		if e.Script == "" {
			return fmt.Errorf("entry %q: script is required", e.ID)
		}
		switch e.Ready.Type {
		case "http", "tcp", "log", "sleep", "none":
		default:
			return fmt.Errorf("entry %q: invalid ready.type %q", e.ID, e.Ready.Type)
		}
		if e.Ready.Type == "log" && e.Ready.Pattern == "" {
			return fmt.Errorf("entry %q: ready.type=log requires pattern", e.ID)
		}
		if e.TTL != nil && *e.TTL < 0 {
			return fmt.Errorf("entry %q: ttl must be >= 0", e.ID)
		}
		models := append([]string{e.ID}, e.Aliases...)
		for _, m := range models {
			if m == "" {
				continue
			}
			if owner, ok := seenModel[m]; ok && owner != e.ID {
				return fmt.Errorf("model name %q used by both %q and %q", m, owner, e.ID)
			}
			seenModel[m] = e.ID
		}
	}
	switch c.Budgets.OnNoFit {
	case "queue", "reject":
	default:
		return fmt.Errorf("budgets.on_no_fit must be queue or reject, got %q", c.Budgets.OnNoFit)
	}
	return nil
}

func expandHome(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// Clone returns a deep copy of the entry.
func (e *Entry) Clone() *Entry {
	c := *e
	c.Aliases = append([]string(nil), e.Aliases...)
	c.Args = append([]string(nil), e.Args...)
	c.Env = append([]string(nil), e.Env...)
	c.TogetherWith = append([]string(nil), e.TogetherWith...)
	c.MutexWith = append([]string(nil), e.MutexWith...)
	c.Tags = append([]string(nil), e.Tags...)
	if e.TTL != nil {
		v := *e.TTL
		c.TTL = &v
	}
	if e.Evictable != nil {
		v := *e.Evictable
		c.Evictable = &v
	}
	return &c
}

// Save writes the config atomically, keeping a .bak copy of the previous file.
func Save(path string, c *Config) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	c.Normalize()
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o644)
	}
	tmp, err := os.CreateTemp(dir, ".entries-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// WriteEntryFile writes a single entry as YAML (used for the trash).
func WriteEntryFile(path string, e *Entry) error {
	b, err := yaml.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
