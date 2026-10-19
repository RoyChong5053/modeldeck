package sched

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"modeldeck/internal/config"
	"modeldeck/internal/runtime"
)

type reservation struct {
	kind  string // starting | stopping
	entry *config.Entry
	vram  int64
	ram   int64
	port  int
}

// NoFitError is returned when an entry cannot be started under the current
// constraints and the queue/reject policy says to give up.
type NoFitError struct{ Reason string }

func (e *NoFitError) Error() string { return "cannot start: " + e.Reason }

// Scheduler enforces the three-layer constraints (port, resources, preferences)
// and owns the idle-TTL reaper.
type Scheduler struct {
	cfg *config.Config
	sup *runtime.Supervisor

	mu       sync.Mutex
	waitCh   chan struct{}
	reserved map[string]reservation
}

func New(cfg *config.Config, sup *runtime.Supervisor) *Scheduler {
	s := &Scheduler{
		cfg:      cfg,
		sup:      sup,
		waitCh:   make(chan struct{}),
		reserved: map[string]reservation{},
	}
	sup.OnStateChange = func(string) {
		s.mu.Lock()
		s.broadcastLocked()
		s.mu.Unlock()
	}
	return s
}

func (s *Scheduler) broadcastLocked() {
	close(s.waitCh)
	s.waitCh = make(chan struct{})
}

// Acquire ensures the entry is Ready, holding one in-flight slot until Release.
func (s *Scheduler) Acquire(ctx context.Context, e *config.Entry) (*runtime.Runtime, error) {
	var deadline time.Time
	if q := s.cfg.Budgets.QueueTimeoutS; q > 0 {
		deadline = time.Now().Add(time.Duration(q) * time.Second)
	}

	for {
		s.mu.Lock()

		if _, busy := s.reserved[e.ID]; busy {
			ch := s.waitCh
			s.mu.Unlock()
			if err := waitCh(ctx, ch, remaining(deadline)); err != nil {
				return nil, err
			}
			continue
		}

		if rt := s.sup.Get(e.ID); rt != nil && rt.State() == runtime.StateReady {
			rt.IncInflight()
			s.mu.Unlock()
			return rt, nil
		}

		victims, ok, reason := s.planLocked(e)
		if !ok {
			if s.cfg.Budgets.OnNoFit == "reject" {
				s.mu.Unlock()
				return nil, &NoFitError{Reason: reason}
			}
			ch := s.waitCh
			s.mu.Unlock()
			if err := waitCh(ctx, ch, remaining(deadline)); err != nil {
				return nil, err
			}
			continue
		}

		s.reserved[e.ID] = reservation{
			kind: "starting", entry: e,
			vram: entryVRAM(e, s.sup.Get(e.ID)), ram: entryRAM(e, s.sup.Get(e.ID)), port: e.Port,
		}
		for _, v := range victims {
			s.reserved[v.id] = reservation{kind: "stopping", entry: v.entry, vram: v.vram, ram: v.ram, port: v.port}
		}
		s.broadcastLocked()
		s.mu.Unlock()

		for _, v := range victims {
			_ = s.sup.Stop(v.id)
			s.mu.Lock()
			delete(s.reserved, v.id)
			s.broadcastLocked()
			s.mu.Unlock()
		}

		rt, err := s.sup.Start(ctx, e)

		s.mu.Lock()
		delete(s.reserved, e.ID)
		s.broadcastLocked()
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		rt.IncInflight()
		return rt, nil
	}
}

// Ensure warms an entry without holding a request slot.
func (s *Scheduler) Ensure(ctx context.Context, e *config.Entry) error {
	rt, err := s.Acquire(ctx, e)
	if err != nil {
		return err
	}
	s.Release(rt)
	return nil
}

func (s *Scheduler) Release(rt *runtime.Runtime) {
	if rt == nil {
		return
	}
	rt.DecInflight()
	rt.Touch()
	s.mu.Lock()
	s.broadcastLocked()
	s.mu.Unlock()
}

// Stop stops an entry through the scheduler so waiters are woken.
func (s *Scheduler) Stop(id string) {
	rt := s.sup.Get(id)
	if rt == nil {
		return
	}
	s.mu.Lock()
	if _, busy := s.reserved[id]; busy {
		s.mu.Unlock()
		return
	}
	s.reserved[id] = reservation{kind: "stopping", entry: rt.Entry, vram: liveVRAM(rt), ram: liveRAM(rt), port: rt.Entry.Port}
	s.broadcastLocked()
	s.mu.Unlock()

	_ = s.sup.Stop(id)

	s.mu.Lock()
	delete(s.reserved, id)
	s.broadcastLocked()
	s.mu.Unlock()
}

// Run starts the idle-TTL reaper until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reap()
		}
	}
}

func (s *Scheduler) reap() {
	def := s.cfg.Budgets.TTLDefaultS
	for _, rt := range s.sup.List() {
		if rt.State() != runtime.StateReady {
			continue
		}
		e := rt.Entry
		if e.Pinned || rt.Inflight() > 0 {
			continue
		}
		ttl := e.TTLSeconds(def)
		if ttl <= 0 {
			continue
		}
		if time.Since(rt.LastUsed()) < time.Duration(ttl)*time.Second {
			continue
		}
		s.mu.Lock()
		if _, busy := s.reserved[e.ID]; busy {
			s.mu.Unlock()
			continue
		}
		s.reserved[e.ID] = reservation{kind: "stopping", entry: e, vram: liveVRAM(rt), ram: liveRAM(rt), port: e.Port}
		s.broadcastLocked()
		s.mu.Unlock()

		_ = s.sup.Stop(e.ID)

		s.mu.Lock()
		delete(s.reserved, e.ID)
		s.broadcastLocked()
		s.mu.Unlock()
	}
}

type occupant struct {
	id    string
	entry *config.Entry
	rt    *runtime.Runtime
	kind  string // running | starting | stopping
	vram  int64
	ram   int64
	port  int
	idle  bool
	last  time.Time
}

func (s *Scheduler) occupantsLocked() []occupant {
	var out []occupant
	seen := map[string]bool{}
	for _, rt := range s.sup.List() {
		st := rt.State()
		if st != runtime.StateReady && st != runtime.StateStarting {
			continue
		}
		o := occupant{
			id: rt.Entry.ID, entry: rt.Entry, rt: rt,
			kind: "running", port: rt.Entry.Port,
			idle: rt.Inflight() == 0, last: rt.LastUsed(),
			vram: liveVRAM(rt), ram: liveRAM(rt),
		}
		if st == runtime.StateStarting {
			o.kind = "starting"
			o.idle = false
		}
		out = append(out, o)
		seen[o.id] = true
	}
	for id, r := range s.reserved {
		if seen[id] {
			for i := range out {
				if out[i].id == id {
					out[i].idle = false
					if r.kind == "stopping" {
						out[i].kind = "stopping"
					}
				}
			}
			continue
		}
		out = append(out, occupant{
			id: id, entry: r.entry, kind: r.kind, port: r.port,
			vram: r.vram, ram: r.ram, idle: false, last: time.Now(),
		})
		seen[id] = true
	}
	return out
}

// planLocked decides which occupants must be evicted to start e, and whether
// that is feasible without interrupting in-flight requests.
func (s *Scheduler) planLocked(e *config.Entry) ([]occupant, bool, string) {
	occ := s.occupantsLocked()
	victim := map[string]bool{}
	add := func(o occupant) {
		if o.id != e.ID {
			victim[o.id] = true
		}
	}

	if e.Port > 0 {
		for _, o := range occ {
			if o.port == e.Port {
				add(o)
			}
		}
	}
	for _, o := range occ {
		if contains(e.MutexWith, o.id) || contains(o.entry.MutexWith, e.ID) {
			add(o)
		}
	}
	if e.Exclusive {
		for _, o := range occ {
			add(o)
		}
	}

	// Hard victims must be stoppable right now.
	for _, o := range occ {
		if !victim[o.id] {
			continue
		}
		if o.kind == "starting" || o.kind == "stopping" {
			return nil, false, fmt.Sprintf("%s is starting or stopping", o.id)
		}
		if !o.entry.EvictableOK() || o.entry.Pinned {
			return nil, false, fmt.Sprintf("%s is not evictable", o.id)
		}
		if !o.idle {
			return nil, false, fmt.Sprintf("%s is serving a request", o.id)
		}
	}

	// Start-concurrency cap.
	starting := 0
	for _, o := range occ {
		if o.kind == "starting" {
			starting++
		}
	}
	if starting+1 > s.cfg.Budgets.MaxStarting {
		return nil, false, "start concurrency limit reached"
	}

	// Resource budgets.
	var (
		useV, useR       bool
		budgetV, budgetR int64
		usageV, usageR   int64
	)
	if gpu := s.sup.GPU(); gpu.OK && gpu.TotalMB > 0 {
		useV = true
		budgetV = gpu.TotalMB - int64(s.cfg.Budgets.VRAMReserveMB)
	}
	if total := runtime.MemTotalMB(); total > 0 {
		useR = true
		budgetR = total - int64(s.cfg.Budgets.RAMReserveMB)
	}
	for _, o := range occ {
		if victim[o.id] {
			continue
		}
		usageV += o.vram
		usageR += o.ram
	}
	ev := entryVRAM(e, s.sup.Get(e.ID))
	er := entryRAM(e, s.sup.Get(e.ID))

	for (useV && usageV+ev > budgetV) || (useR && usageR+er > budgetR) {
		cand, found := s.pickVictim(occ, victim, e)
		if !found {
			return nil, false, "insufficient memory and no idle evictable entry"
		}
		victim[cand.id] = true
		usageV -= cand.vram
		usageR -= cand.ram
	}

	var victims []occupant
	for _, o := range occ {
		if victim[o.id] {
			victims = append(victims, o)
		}
	}
	return victims, true, ""
}

func (s *Scheduler) pickVictim(occ []occupant, victim map[string]bool, e *config.Entry) (occupant, bool) {
	var cands []occupant
	for _, o := range occ {
		if victim[o.id] || o.id == e.ID {
			continue
		}
		if o.kind != "running" || !o.idle {
			continue
		}
		if !o.entry.EvictableOK() || o.entry.Pinned {
			continue
		}
		if contains(e.TogetherWith, o.id) {
			continue
		}
		cands = append(cands, o)
	}
	if len(cands) == 0 {
		return occupant{}, false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].entry.Priority != cands[j].entry.Priority {
			return cands[i].entry.Priority < cands[j].entry.Priority
		}
		return cands[i].last.Before(cands[j].last)
	})
	return cands[0], true
}

func liveVRAM(rt *runtime.Runtime) int64 {
	if rt == nil {
		return 0
	}
	if v := rt.CurVRAMMB(); v > 0 {
		return v
	}
	if v := rt.PeakVRAMMB(); v > 0 {
		return v
	}
	return int64(rt.Entry.Resources.VRAMMB)
}

func liveRAM(rt *runtime.Runtime) int64 {
	if rt == nil {
		return 0
	}
	if v := rt.CurRSSKBVal(); v > 0 {
		return v / 1024
	}
	if v := rt.PeakRSSKBVal(); v > 0 {
		return v / 1024
	}
	return int64(rt.Entry.Resources.RAMMB)
}

func entryVRAM(e *config.Entry, rt *runtime.Runtime) int64 {
	if rt != nil {
		if v := rt.CurVRAMMB(); v > 0 {
			return v
		}
		if v := rt.PeakVRAMMB(); v > 0 {
			return v
		}
	}
	return int64(e.Resources.VRAMMB)
}

func entryRAM(e *config.Entry, rt *runtime.Runtime) int64 {
	if rt != nil {
		if v := rt.CurRSSKBVal(); v > 0 {
			return v / 1024
		}
		if v := rt.PeakRSSKBVal(); v > 0 {
			return v / 1024
		}
	}
	return int64(e.Resources.RAMMB)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func remaining(deadline time.Time) *time.Duration {
	if deadline.IsZero() {
		return nil
	}
	d := time.Until(deadline)
	return &d
}

func waitCh(ctx context.Context, ch <-chan struct{}, max *time.Duration) error {
	var (
		timer *time.Timer
		tc    <-chan time.Time
	)
	if max != nil {
		if *max <= 0 {
			return &NoFitError{Reason: "queue timeout"}
		}
		timer = time.NewTimer(*max)
		tc = timer.C
		defer timer.Stop()
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-tc:
		return &NoFitError{Reason: "queue timeout"}
	}
}
