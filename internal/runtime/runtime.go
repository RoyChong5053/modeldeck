package runtime

import (
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"modeldeck/internal/config"
)

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

// Runtime is the live state of one entry.
type Runtime struct {
	Entry *config.Entry

	mu        sync.Mutex
	state     State
	cmd       *exec.Cmd
	pid       int
	pgid      int
	startedAt time.Time
	lastUsed  time.Time
	lastErr   string

	inflight atomic.Int64
	curRSS   atomic.Int64
	peakRSS  atomic.Int64
	curVRAM  atomic.Int64
	peakVRAM atomic.Int64

	logs    *LogBuffer
	logPath string
	logFile *os.File

	readyCh  chan struct{}
	doneCh   chan struct{}
	readyErr string
}

func NewRuntime(e *config.Entry, logPath string) *Runtime {
	return &Runtime{
		Entry:   e,
		state:   StateStopped,
		logs:    NewLogBuffer(2000),
		logPath: logPath,
	}
}

func (rt *Runtime) State() State {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.state
}

func (rt *Runtime) Logs() *LogBuffer { return rt.logs }

func (rt *Runtime) LogPath() string { return rt.logPath }

func (rt *Runtime) Inflight() int64 { return rt.inflight.Load() }

func (rt *Runtime) CurVRAMMB() int64 { return rt.curVRAM.Load() }

func (rt *Runtime) PeakVRAMMB() int64 { return rt.peakVRAM.Load() }

func (rt *Runtime) CurRSSKBVal() int64 { return rt.curRSS.Load() }

func (rt *Runtime) PeakRSSKBVal() int64 { return rt.peakRSS.Load() }

func (rt *Runtime) IncInflight() int64 { return rt.inflight.Add(1) }

func (rt *Runtime) DecInflight() int64 { return rt.inflight.Add(-1) }

func (rt *Runtime) Touch() {
	rt.mu.Lock()
	rt.lastUsed = time.Now()
	rt.mu.Unlock()
}

func (rt *Runtime) LastUsed() time.Time {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.lastUsed
}

func (rt *Runtime) Pid() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.pid
}

func (rt *Runtime) Pgid() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.pgid
}

// Snapshot is a JSON-friendly view for the API/UI.
type Snapshot struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Script    string   `json:"script"`
	Mode      string   `json:"mode"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Aliases   []string `json:"aliases"`
	Tags      []string `json:"tags"`
	Placement string   `json:"placement"`
	State     State    `json:"state"`
	TTL       int      `json:"ttl"`
	Pinned    bool     `json:"pinned"`
	Evictable bool     `json:"evictable"`
	Exclusive bool     `json:"exclusive"`
	Priority  int      `json:"priority"`

	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	LastUsed  time.Time `json:"last_used"`
	Inflight  int64     `json:"inflight"`

	CurRSSKB  int64 `json:"cur_rss_kb"`
	PeakRSSKB int64 `json:"peak_rss_kb"`
	CurVRAMMB int64 `json:"cur_vram_mb"`
	PeakVRAM  int64 `json:"peak_vram_mb"`

	ResVRAMMB int `json:"res_vram_mb"`
	ResRAMMB  int `json:"res_ram_mb"`

	LastErr string `json:"last_error,omitempty"`
	LogPath string `json:"log_path"`
}

func (rt *Runtime) Snapshot(defTTL int) Snapshot {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return Snapshot{
		ID:        rt.Entry.ID,
		Name:      rt.Entry.DisplayName(),
		Script:    rt.Entry.Script,
		Mode:      rt.Entry.Mode,
		Host:      rt.Entry.HostOr(),
		Port:      rt.Entry.Port,
		Aliases:   append([]string(nil), rt.Entry.Aliases...),
		Tags:      append([]string(nil), rt.Entry.Tags...),
		Placement: rt.Entry.Placement,
		State:     rt.state,
		TTL:       rt.Entry.TTLSeconds(defTTL),
		Pinned:    rt.Entry.Pinned,
		Evictable: rt.Entry.EvictableOK(),
		Exclusive: rt.Entry.Exclusive,
		Priority:  rt.Entry.Priority,

		PID:       rt.pid,
		StartedAt: rt.startedAt,
		LastUsed:  rt.lastUsed,
		Inflight:  rt.inflight.Load(),

		CurRSSKB:  rt.curRSS.Load(),
		PeakRSSKB: rt.peakRSS.Load(),
		CurVRAMMB: rt.curVRAM.Load(),
		PeakVRAM:  rt.peakVRAM.Load(),

		ResVRAMMB: rt.Entry.Resources.VRAMMB,
		ResRAMMB:  rt.Entry.Resources.RAMMB,

		LastErr: rt.lastErr,
		LogPath: rt.logPath,
	}
}
