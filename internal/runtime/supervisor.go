package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"modeldeck/internal/config"
)

// Supervisor owns the lifecycle of every entry's process.
type Supervisor struct {
	cfg      *config.Config
	logDir   string
	ownerPID int

	mu  sync.Mutex
	rts map[string]*Runtime

	memAvailable atomic.Int64
	gpu          atomic.Pointer[GPUStats]

	// OnStateChange is called (without locks held) whenever a runtime's state
	// changes. The scheduler uses it to wake waiters.
	OnStateChange func(id string)
}

// GPU returns the most recently sampled GPU stats.
func (s *Supervisor) GPU() GPUStats {
	if p := s.gpu.Load(); p != nil {
		return *p
	}
	return ReadGPU()
}

func NewSupervisor(cfg *config.Config) (*Supervisor, error) {
	logDir := filepath.Join(cfg.Server.StateDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	s := &Supervisor{
		cfg:      cfg,
		logDir:   logDir,
		ownerPID: os.Getpid(),
		rts:      map[string]*Runtime{},
	}
	for _, e := range cfg.Entries {
		s.rts[e.ID] = NewRuntime(e, filepath.Join(logDir, e.ID+".log"))
	}
	s.cleanupOrphans()
	go s.sampleLoop()
	return s, nil
}

func (s *Supervisor) Get(id string) *Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rts[id]
}

// EnsureRuntime makes sure a Runtime exists for the entry and refreshes its
// config pointer. It does not start anything.
func (s *Supervisor) EnsureRuntime(e *config.Entry) *Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt, ok := s.rts[e.ID]; ok {
		rt.mu.Lock()
		rt.Entry = e
		rt.mu.Unlock()
		return rt
	}
	rt := NewRuntime(e, filepath.Join(s.logDir, e.ID+".log"))
	s.rts[e.ID] = rt
	return rt
}

// SyncRuntimes reconciles runtimes with the given entry set: entries that
// disappeared are stopped and dropped, the rest are ensured.
func (s *Supervisor) SyncRuntimes(entries []*config.Entry) {
	want := make(map[string]bool, len(entries))
	for _, e := range entries {
		want[e.ID] = true
	}
	for _, rt := range s.List() {
		if !want[rt.Entry.ID] {
			_ = s.Stop(rt.Entry.ID)
		}
	}
	s.mu.Lock()
	for id := range s.rts {
		if !want[id] {
			delete(s.rts, id)
		}
	}
	s.mu.Unlock()
	for _, e := range entries {
		s.EnsureRuntime(e)
	}
}

func (s *Supervisor) List() []*Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Runtime, 0, len(s.rts))
	for _, rt := range s.rts {
		out = append(out, rt)
	}
	return out
}

func (s *Supervisor) MemAvailableMB() int64 {
	if v := s.memAvailable.Load(); v > 0 {
		return v
	}
	return MemAvailableMB()
}

func (s *Supervisor) notify(id string) {
	if s.OnStateChange != nil {
		s.OnStateChange(id)
	}
}

// Start brings an entry to Ready and blocks until it is serving (or fails).
func (s *Supervisor) Start(ctx context.Context, e *config.Entry) (*Runtime, error) {
	rt := s.Get(e.ID)
	if rt == nil {
		rt = NewRuntime(e, filepath.Join(s.logDir, e.ID+".log"))
		s.mu.Lock()
		s.rts[e.ID] = rt
		s.mu.Unlock()
	}

	for {
		rt.mu.Lock()
		switch rt.state {
		case StateReady:
			rt.mu.Unlock()
			return rt, nil
		case StateStarting:
			ch := rt.readyCh
			rt.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case StateStopping:
			ch := rt.doneCh
			rt.mu.Unlock()
			if ch != nil {
				select {
				case <-ch:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			continue
		}
		// Stopped or Failed -> (re)start.
		if err := s.checkPortFree(e); err != nil {
			rt.state = StateFailed
			rt.lastErr = err.Error()
			rt.mu.Unlock()
			s.notify(e.ID)
			return nil, err
		}
		rt.state = StateStarting
		rt.readyErr = ""
		rt.lastErr = ""
		rt.readyCh = make(chan struct{})
		rt.doneCh = make(chan struct{})
		rt.startedAt = time.Now()
		rt.lastUsed = time.Now()
		rt.curRSS.Store(0)
		rt.peakRSS.Store(0)
		rt.curVRAM.Store(0)
		rt.peakVRAM.Store(0)
		rt.mu.Unlock()
		break
	}

	s.notify(e.ID)
	if err := s.spawn(e, rt); err != nil {
		rt.mu.Lock()
		rt.state = StateFailed
		rt.lastErr = err.Error()
		rt.mu.Unlock()
		rt.closeReady()
		rt.closeDone()
		s.notify(e.ID)
		return nil, err
	}

	rt.mu.Lock()
	ch := rt.readyCh
	rt.mu.Unlock()

	timeout := time.Duration(e.Ready.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-ctx.Done():
		_ = s.StopWithGrace(e.ID, 5*time.Second)
		return nil, ctx.Err()
	case <-timer.C:
		_ = s.StopWithGrace(e.ID, 5*time.Second)
		return nil, fmt.Errorf("[%s] readiness timed out after %s", e.ID, timeout)
	}

	rt.mu.Lock()
	st := rt.state
	errStr := rt.readyErr
	rt.mu.Unlock()
	if st != StateReady {
		if errStr == "" {
			errStr = "start failed"
		}
		return nil, fmt.Errorf("[%s] %s", e.ID, errStr)
	}
	rt.Touch()
	return rt, nil
}

func (s *Supervisor) spawn(e *config.Entry, rt *Runtime) error {
	cmd := exec.Command(e.Script, e.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e.Cwd != "" {
		cmd.Dir = e.Cwd
	}
	env := append([]string{}, os.Environ()...)
	env = append(env, e.Env...)
	env = append(env, "DECK_ENTRY="+e.ID, "DECK_PID="+strconv.Itoa(s.ownerPID))
	if e.Port > 0 {
		env = append(env, "DECK_PORT="+strconv.Itoa(e.Port), "DECK_URL="+e.BaseURL())
	}
	cmd.Env = env

	lf, err := os.OpenFile(rt.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	writers := []io.Writer{rt.logs}
	if err == nil {
		lf.Truncate(0)
		_, _ = lf.Seek(0, io.SeekStart)
		rt.logFile = lf
		writers = append(writers, lf)
	}
	cmd.Stdout = io.MultiWriter(writers...)
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		if rt.logFile != nil {
			rt.logFile.Close()
			rt.logFile = nil
		}
		return fmt.Errorf("[%s] failed to start %s: %w", e.ID, e.Script, err)
	}

	rt.mu.Lock()
	rt.cmd = cmd
	rt.pid = cmd.Process.Pid
	rt.pgid = cmd.Process.Pid
	rt.mu.Unlock()

	go s.waitProc(rt, cmd)
	go s.probe(rt, e)
	return nil
}

func (s *Supervisor) waitProc(rt *Runtime, cmd *exec.Cmd) {
	err := cmd.Wait()

	rt.mu.Lock()
	wasStopping := rt.state == StateStopping
	starting := rt.state == StateStarting
	rt.cmd = nil
	rt.pid = 0
	if wasStopping {
		rt.state = StateStopped
	} else if err != nil {
		rt.state = StateFailed
		rt.lastErr = err.Error()
	} else {
		rt.state = StateStopped
	}
	if starting && rt.readyErr == "" {
		rt.readyErr = "process exited before becoming ready"
		if rt.lastErr == "" {
			rt.lastErr = rt.readyErr
		}
	}
	lf := rt.logFile
	rt.logFile = nil
	rt.mu.Unlock()

	if lf != nil {
		lf.Close()
	}
	rt.closeReady()
	rt.closeDone()

	// Feed measured resources back into the config (unless pinned manually).
	if !rt.Entry.ManualRes {
		if peak := rt.peakVRAM.Load(); peak > 0 {
			rt.Entry.Resources.VRAMMB = int(peak)
		}
		if peak := rt.peakRSS.Load(); peak > 0 {
			rt.Entry.Resources.RAMMB = int(peak / 1024)
		}
	}
	s.notify(rt.Entry.ID)
}

func (s *Supervisor) probe(rt *Runtime, e *config.Entry) {
	rt.mu.Lock()
	if rt.state != StateStarting {
		rt.mu.Unlock()
		return
	}
	deadline := time.Now().Add(time.Duration(e.Ready.Timeout) * time.Second)
	rt.mu.Unlock()

	for {
		if rt.State() != StateStarting {
			return
		}
		if s.checkReady(e, rt) {
			rt.mu.Lock()
			if rt.state == StateStarting {
				rt.state = StateReady
				rt.lastUsed = time.Now()
			}
			rt.mu.Unlock()
			rt.closeReady()
			s.notify(e.ID)
			return
		}
		if time.Now().After(deadline) {
			rt.mu.Lock()
			if rt.state == StateStarting {
				rt.state = StateFailed
				rt.readyErr = "readiness timed out"
				rt.lastErr = rt.readyErr
			}
			rt.mu.Unlock()
			rt.closeReady()
			s.notify(e.ID)
			return
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-rt.Done():
			return
		}
	}
}

// Done returns the channel closed when the current process exits.
func (rt *Runtime) Done() <-chan struct{} {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.doneCh
}

func (rt *Runtime) closeReady() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.readyCh != nil {
		select {
		case <-rt.readyCh:
		default:
			close(rt.readyCh)
		}
	}
}

func (rt *Runtime) closeDone() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.doneCh != nil {
		select {
		case <-rt.doneCh:
		default:
			close(rt.doneCh)
		}
	}
}

func (s *Supervisor) checkReady(e *config.Entry, rt *Runtime) bool {
	switch e.Ready.Type {
	case "none":
		rt.mu.Lock()
		started := rt.startedAt
		rt.mu.Unlock()
		return time.Since(started) >= 250*time.Millisecond
	case "sleep":
		rt.mu.Lock()
		started := rt.startedAt
		rt.mu.Unlock()
		return time.Since(started) >= time.Duration(e.Ready.Seconds)*time.Second
	case "tcp":
		if e.Port <= 0 {
			return true
		}
		c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", e.HostOr(), e.Port), 500*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	case "http":
		cl := &http.Client{Timeout: 900 * time.Millisecond}
		resp, err := cl.Get(e.BaseURL() + e.Ready.Path)
		if err != nil {
			return false
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	case "log":
		re, err := regexp.Compile(e.Ready.Pattern)
		if err != nil {
			return false
		}
		for _, l := range rt.logs.Tail(200) {
			if re.MatchString(l) {
				return true
			}
		}
		return false
	}
	return false
}

func (s *Supervisor) checkPortFree(e *config.Entry) error {
	if e.Port <= 0 || e.Mode == "job" {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", e.HostOr(), e.Port))
	if err != nil {
		return fmt.Errorf("[%s] port %d is already in use on %s: %w", e.ID, e.Port, e.HostOr(), err)
	}
	ln.Close()
	return nil
}

func (s *Supervisor) waitPortFree(e *config.Entry, d time.Duration) {
	if e.Port <= 0 || e.Mode == "job" {
		return
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", e.HostOr(), e.Port))
		if err == nil {
			ln.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Stop stops an entry with its configured grace period.
func (s *Supervisor) Stop(id string) error {
	rt := s.Get(id)
	if rt == nil {
		return nil
	}
	return s.StopWithGrace(id, time.Duration(rt.Entry.StopGrace())*time.Second)
}

func (s *Supervisor) StopWithGrace(id string, grace time.Duration) error {
	rt := s.Get(id)
	if rt == nil {
		return nil
	}
	if grace <= 0 {
		grace = time.Duration(rt.Entry.StopGrace()) * time.Second
	}

	rt.mu.Lock()
	if rt.cmd == nil || rt.state == StateStopped || rt.state == StateFailed {
		rt.state = StateStopped
		rt.mu.Unlock()
		return nil
	}
	rt.state = StateStopping
	pid := rt.pid
	done := rt.doneCh
	hook := rt.Entry.Stop.Hook
	rt.mu.Unlock()
	s.notify(id)

	if hook != "" {
		c := exec.Command("sh", "-c", hook)
		c.Env = append(os.Environ(),
			"DECK_ENTRY="+id,
			"DECK_PORT="+strconv.Itoa(rt.Entry.Port),
			"DECK_PID="+strconv.Itoa(pid),
			"DECK_URL="+rt.Entry.BaseURL(),
		)
		doneHook := make(chan struct{})
		go func() { _ = c.Run(); close(doneHook) }()
		select {
		case <-doneHook:
		case <-time.After(grace):
		}
	}

	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(grace):
			if pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	}
	s.waitPortFree(rt.Entry, 5*time.Second)

	rt.mu.Lock()
	rt.state = StateStopped
	rt.mu.Unlock()
	s.notify(id)
	return nil
}

func (s *Supervisor) sampleLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.sampleOnce()
	}
}

func (s *Supervisor) sampleOnce() {
	s.memAvailable.Store(MemAvailableMB())
	gpu := ReadGPU()
	s.gpu.Store(&gpu)

	for _, rt := range s.List() {
		pgid := rt.Pgid()
		if pgid <= 0 {
			continue
		}
		pids := groupPIDs(pgid)
		var rss int64
		var vram int64
		group := map[int]bool{}
		for _, p := range pids {
			group[p] = true
			rss += pidRSSKB(p)
		}
		if gpu.OK {
			for p, mb := range gpu.PerPID {
				if group[p] {
					vram += mb
				}
			}
		}
		rt.curRSS.Store(rss)
		rt.curVRAM.Store(vram)
		if rss > rt.peakRSS.Load() {
			rt.peakRSS.Store(rss)
		}
		if vram > rt.peakVRAM.Load() {
			rt.peakVRAM.Store(vram)
		}
	}
}

func groupPIDs(pgid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(de.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if procGroup(pid) == pgid {
			out = append(out, pid)
		}
	}
	return out
}

// cleanupOrphans kills processes started by a previous deck instance.
func (s *Supervisor) cleanupOrphans() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self := strconv.Itoa(s.ownerPID)
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(de.Name())
		if err != nil || pid <= 0 || pid == s.ownerPID {
			continue
		}
		env, err := os.ReadFile("/proc/" + de.Name() + "/environ")
		if err != nil {
			continue
		}
		var deckEntry, deckPid string
		for _, kv := range strings.Split(string(env), "\x00") {
			switch {
			case strings.HasPrefix(kv, "DECK_ENTRY="):
				deckEntry = strings.TrimPrefix(kv, "DECK_ENTRY=")
			case strings.HasPrefix(kv, "DECK_PID="):
				deckPid = strings.TrimPrefix(kv, "DECK_PID=")
			}
		}
		if deckEntry == "" || deckPid == "" || deckPid == self {
			continue
		}
		pgid := procGroup(pid)
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
}
