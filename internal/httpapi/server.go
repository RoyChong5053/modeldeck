package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"modeldeck/internal/config"
	"modeldeck/internal/proxy"
	"modeldeck/internal/registry"
	"modeldeck/internal/runtime"
	"modeldeck/internal/scan"
	"modeldeck/internal/sched"
	"modeldeck/internal/version"
)

// Server wires the control plane, the data plane, and the WebUI onto one port.
type Server struct {
	cfg   *config.Config
	reg   *registry.Registry
	sup   *runtime.Supervisor
	sch   *sched.Scheduler
	proxy *proxy.Service
	web   http.Handler

	hostname  string
	startedAt time.Time

	// mu guards edits to cfg.Entries (create/rename/delete) and readers of it.
	mu sync.RWMutex
}

func New(cfg *config.Config, reg *registry.Registry, sup *runtime.Supervisor, sch *sched.Scheduler, pr *proxy.Service, web http.Handler) *Server {
	return &Server{
		cfg: cfg, reg: reg, sup: sup, sch: sch, proxy: pr, web: web,
		hostname: runtime.DeviceName(), startedAt: time.Now(),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !s.authorized(w, r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "unauthorized"}})
		return
	}
	// Mutating calls are logged: "who deleted my entry" needs an answer.
	if r.Method != http.MethodGet && strings.HasPrefix(p, "/api/") {
		log.Printf("[api] %s %s from %s", r.Method, p, r.RemoteAddr)
	}
	switch {
	case p == "/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case p == "/" || strings.HasPrefix(p, "/static/"):
		s.web.ServeHTTP(w, r)
	case strings.HasPrefix(p, "/api/"):
		s.control(w, r, strings.TrimPrefix(p, "/api/"))
	case p == "/models" || strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/run/"):
		s.proxy.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// authorized checks the bearer token. The WebUI cannot set headers on
// EventSource/fetch from a plain page, so a request that carries the token
// (header or ?token=) also receives a session cookie the browser replays.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	token := s.cfg.Server.Token
	if token == "" {
		return true
	}
	// A request that carries the token is trusted and hands the browser a
	// cookie — including the page itself, so opening the WebUI as
	// http://host:12688/?token=... is enough to make the whole dashboard work.
	if q := r.URL.Query().Get("token"); q == token {
		s.setSession(w, token)
		return true
	}
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == token {
		s.setSession(w, token)
		return true
	}
	p := r.URL.Path
	if p == "/health" || p == "/" || strings.HasPrefix(p, "/static/") {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value == token {
		return true
	}
	return false
}

const sessionCookie = "deck_session"

// setSession hands the browser a cookie carrying the token, so the WebUI
// works unchanged once a token is configured.
func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 3600,
	})
}

func (s *Server) control(w http.ResponseWriter, r *http.Request, rel string) {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	switch parts[0] {
	case "status":
		writeJSON(w, http.StatusOK, s.statusPayload(r.RemoteAddr))
	case "mem":
		writeJSON(w, http.StatusOK, s.memPayload())
	case "scan":
		s.scanHandler(w, r)
	case "entries":
		if len(parts) == 1 {
			if r.Method == http.MethodPost {
				s.createEntry(w, r)
				return
			}
			s.mu.RLock()
			entries := append([]*config.Entry(nil), s.cfg.Entries...)
			s.mu.RUnlock()
			writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
			return
		}
		id := parts[1]
		if len(parts) == 2 {
			if r.Method == http.MethodDelete {
				s.deleteEntry(w, r, id)
				return
			}
			rt := s.sup.Get(id)
			if rt == nil {
				writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
				return
			}
			writeJSON(w, http.StatusOK, rt.Snapshot(s.cfg.Budgets.TTLDefaultS))
			return
		}
		switch parts[2] {
		case "logs":
			if len(parts) >= 4 && parts[3] == "stream" {
				s.logStream(w, r, id)
				return
			}
			s.logTail(w, r, id)
		case "pin":
			s.pin(w, r, id)
		case "lock":
			s.pin(w, r, id)
		case "reset":
			s.reset(w, r, id)
		case "config":
			s.entryConfig(w, r, id)
		case "ttl":
			s.setTTL(w, r, id)
		case "preset":
			s.preset(w, r, id)
		case "rename":
			s.rename(w, r, id)
		case "start", "stop", "restart":
			s.action(w, r, id, parts[2])
		default:
			writeAPIErr(w, http.StatusNotFound, "unknown action "+parts[2])
		}
	default:
		writeAPIErr(w, http.StatusNotFound, "unknown endpoint /api/"+rel)
	}
}

func (s *Server) statusPayload(remoteAddr string) map[string]any {
	def := s.cfg.Budgets.TTLDefaultS
	rts := s.sup.List()
	snaps := make([]runtime.Snapshot, 0, len(rts))
	for _, rt := range rts {
		snaps = append(snaps, rt.Snapshot(def))
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
	payload := s.memPayload()
	payload["entries"] = snaps
	payload["budgets"] = map[string]any{
		"vram_reserve_mb":       s.cfg.Budgets.VRAMReserveMB,
		"ram_reserve_mb":        s.cfg.Budgets.RAMReserveMB,
		"max_starting":          s.cfg.Budgets.MaxStarting,
		"on_no_fit":             s.cfg.Budgets.OnNoFit,
		"queue_timeout":         s.cfg.Budgets.QueueTimeoutS,
		"ttl_default":           s.cfg.Budgets.TTLDefaultS,
		"serialize_unknown_gpu": s.cfg.Budgets.SerializeUnknownGPUOn(),
	}
	payload["listen"] = s.cfg.Server.Listen
	payload["host"] = s.hostPayload(remoteAddr)
	return payload
}

// hostPayload describes the machine deck runs on: identity plus live hardware
// status, so a browser with five deck tabs open can tell them apart.
func (s *Server) hostPayload(remoteAddr string) map[string]any {
	gpu := s.sup.GPU()
	mem := runtime.ReadMem()
	cpu := runtime.ReadCPU()
	return map[string]any{
		"hostname":      s.hostname,
		"label":         s.cfg.Server.Label,
		"version":       version.Version,
		"uptime_s":      int64(time.Since(s.startedAt).Seconds()),
		"host_uptime_s": runtime.ReadUptime(),
		"listen":        s.cfg.Server.Listen,
		"gpu": map[string]any{
			"ok":       gpu.OK,
			"name":     gpu.Name,
			"used_mb":  gpu.UsedMB,
			"free_mb":  gpu.FreeMB,
			"total_mb": gpu.TotalMB,
			"util_pct": gpu.UtilPct,
			"temp_c":   gpu.TempC,
		},
		"cpu": map[string]any{
			"model": cpu.Model,
			"cores": cpu.Cores,
			"load1": cpu.Load1,
		},
		"mem": map[string]any{
			"total_mb":     mem.TotalMB,
			"available_mb": mem.AvailableMB,
			"used_mb":      mem.UsedMB,
		},
		// The addresses other machines can open: with a wildcard bind this is
		// every local address (LAN + Tailscale), so a phone can be handed a
		// link that works.
		"urls": runtime.SortURLsByProximity(runtime.ListenURLs(s.cfg.Server.Listen, ""), remoteAddr),
	}
}

func (s *Server) memPayload() map[string]any {
	gpu := s.sup.GPU()
	return map[string]any{
		"mem": map[string]any{
			"available_mb": s.sup.MemAvailableMB(),
			"total_mb":     runtime.MemTotalMB(),
		},
		"gpu": map[string]any{
			"ok":       gpu.OK,
			"free_mb":  gpu.FreeMB,
			"total_mb": gpu.TotalMB,
		},
	}
}

func (s *Server) action(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	e, ok := s.reg.ByID(id)
	if !ok {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	run := func(ctx context.Context) error {
		switch action {
		case "start":
			return s.sch.Ensure(ctx, e)
		case "stop":
			s.sch.Stop(id)
			return nil
		case "restart":
			s.sch.Stop(id)
			return s.sch.Ensure(ctx, e)
		}
		return fmt.Errorf("unknown action %q", action)
	}

	if r.URL.Query().Get("async") == "1" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.actionTimeout(e))
			defer cancel()
			if err := run(ctx); err != nil {
				fmt.Printf("[api] %s %s: %v\n", action, id, err)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "id": id, "action": action})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.actionTimeout(e))
	defer cancel()
	if err := run(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "id": id, "action": action})
}

func (s *Server) actionTimeout(e *config.Entry) time.Duration {
	t := time.Duration(e.Ready.Timeout) * time.Second
	t += time.Duration(s.cfg.Budgets.QueueTimeoutS) * time.Second
	return t + 30*time.Second
}

// pin locks an entry in place: a locked entry is never unloaded by the idle
// reaper and never evicted to make room for another model. That is what you
// want while a model is under test (ASR+TTS, a long generation) and the
// warm-up cost would show up as latency on every request.
//
// The flag is persisted, so it survives a deck restart.
func (s *Server) pin(w http.ResponseWriter, r *http.Request, id string) {
	e, ok := s.reg.ByID(id)
	if !ok {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var body struct {
		Pinned *bool `json:"pinned"`
		Locked *bool `json:"locked"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	want := !e.Pinned
	switch {
	case body.Pinned != nil:
		want = *body.Pinned
	case body.Locked != nil:
		want = *body.Locked
	}
	if err := s.commitEntries(func(list []*config.Entry) ([]*config.Entry, error) {
		for _, x := range list {
			if x.ID == id {
				x.Pinned = want
			}
		}
		return list, nil
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "pinned": want, "locked": want})
}

// reset clears a failed state so a stale error does not stick to the entry
// forever. Nothing is restarted; the entry simply goes back to "stopped".
func (s *Server) reset(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	rt := s.sup.Get(id)
	if rt == nil {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	cleared := rt.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "cleared": cleared})
}

// entryConfigReq is the editable subset of an entry. Every field is a
// pointer, so "not sent" and "set to the zero value" stay distinguishable.
type entryConfigReq struct {
	Name          *string   `json:"name"`
	Script        *string   `json:"script"`
	Args          *[]string `json:"args"`
	Env           *[]string `json:"env"`
	Cwd           *string   `json:"cwd"`
	Mode          *string   `json:"mode"`
	Host          *string   `json:"host"`
	Port          *int      `json:"port"`
	TTL           *int      `json:"ttl"`
	Placement     *string   `json:"placement"`
	Tags          *[]string `json:"tags"`
	UpstreamModel *string   `json:"upstream_model"`
	ReadyPath     *string   `json:"ready_path"`
	ReadyTimeout  *int      `json:"ready_timeout"`
}

type entryConfigResp struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Script        string           `json:"script"`
	Args          []string         `json:"args"`
	Env           []string         `json:"env"`
	Cwd           string           `json:"cwd"`
	Mode          string           `json:"mode"`
	Host          string           `json:"host"`
	Port          int              `json:"port"`
	TTL           *int             `json:"ttl"`
	Placement     string           `json:"placement"`
	Tags          []string         `json:"tags"`
	UpstreamModel string           `json:"upstream_model"`
	Ready         config.Ready     `json:"ready"`
	Resources     config.Resources `json:"resources"`
	Pinned        bool             `json:"pinned"`
	Running       bool             `json:"running"`
	Parsed        *scan.Candidate  `json:"parsed,omitempty"`
}

// entryConfigPayload is the JSON view of an entry used by the WebUI editor.
func (s *Server) entryConfigPayload(e *config.Entry) entryConfigResp {
	out := entryConfigResp{
		ID:            e.ID,
		Name:          e.Name,
		Script:        e.Script,
		Args:          e.Args,
		Env:           e.Env,
		Cwd:           e.Cwd,
		Mode:          e.Mode,
		Host:          e.HostOr(),
		Port:          e.Port,
		TTL:           e.TTL,
		Placement:     e.Placement,
		Tags:          e.Tags,
		UpstreamModel: e.UpstreamModel,
		Ready:         e.Ready,
		Resources:     e.Resources,
		Pinned:        e.Pinned,
	}
	if rt := s.sup.Get(e.ID); rt != nil {
		st := rt.State()
		out.Running = st == runtime.StateReady || st == runtime.StateStarting
	}
	if c, err := scan.ParseScript(e.Script); err == nil {
		out.Parsed = c
	}
	return out
}

// entryConfig reads (GET) or edits (POST) the fields of one entry. Editing
// the start script here and pressing 启动 in the same screen is the fastest
// way to try a new model without touching a YAML file.
func (s *Server) entryConfig(w http.ResponseWriter, r *http.Request, id string) {
	e, ok := s.reg.ByID(id)
	if !ok {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.entryConfigPayload(e))
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeAPIErr(w, http.StatusMethodNotAllowed, "GET or POST required")
		return
	}

	var req entryConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Validate before touching the config: a typo in the script path must not
	// leave the entry half-edited.
	newScript := e.Script
	if req.Script != nil {
		newScript = config.ExpandHome(strings.TrimSpace(*req.Script))
		if newScript == "" {
			writeAPIErr(w, http.StatusBadRequest, "script must not be empty")
			return
		}
		if _, err := os.Stat(newScript); err != nil {
			writeAPIErr(w, http.StatusBadRequest, "script not found: "+newScript)
			return
		}
	}
	if req.Port != nil && (*req.Port < 1 || *req.Port > 65535) {
		writeAPIErr(w, http.StatusBadRequest, "port must be 1..65535")
		return
	}
	if req.TTL != nil && *req.TTL < -1 {
		writeAPIErr(w, http.StatusBadRequest, "ttl must be >= 0 (-1 = use the default)")
		return
	}

	// A new script usually carries its own --port; follow it so the editor
	// does not have to be told twice.
	var parsed *scan.Candidate
	if newScript != e.Script {
		if c, err := scan.ParseScript(newScript); err == nil {
			parsed = c
		}
	}

	err := s.commitEntries(func(list []*config.Entry) ([]*config.Entry, error) {
		for i, x := range list {
			if x.ID != id {
				continue
			}
			ne := x.Clone()
			if req.Name != nil {
				ne.Name = *req.Name
			}
			if req.Script != nil {
				ne.Script = newScript
			}
			if req.Args != nil {
				ne.Args = *req.Args
			}
			if req.Env != nil {
				ne.Env = *req.Env
			}
			if req.Cwd != nil {
				ne.Cwd = config.ExpandHome(*req.Cwd)
			}
			if req.Mode != nil {
				ne.Mode = *req.Mode
			}
			if req.Host != nil {
				ne.Host = *req.Host
			}
			if req.Port != nil {
				ne.Port = *req.Port
			}
			if req.TTL != nil {
				if *req.TTL < 0 {
					ne.TTL = nil // back to the deck-wide default
				} else {
					v := *req.TTL
					ne.TTL = &v
				}
			}
			if req.Placement != nil {
				ne.Placement = *req.Placement
			}
			if req.Tags != nil {
				ne.Tags = *req.Tags
			}
			if req.UpstreamModel != nil {
				ne.UpstreamModel = *req.UpstreamModel
			}
			if req.ReadyPath != nil {
				ne.Ready.Path = *req.ReadyPath
			}
			if req.ReadyTimeout != nil {
				ne.Ready.Timeout = *req.ReadyTimeout
			}
			if req.Port == nil && parsed != nil && parsed.Port > 0 {
				ne.Port = parsed.Port
			}
			next := append([]*config.Entry(nil), list...)
			next[i] = ne
			return next, nil
		}
		return nil, fmt.Errorf("unknown entry %q", id)
	})
	if err != nil {
		writeAPIErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, _ := s.reg.ByID(id)
	writeJSON(w, http.StatusOK, s.entryConfigPayload(updated))
}

func (s *Server) setTTL(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var body struct {
		TTL *int `json:"ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TTL == nil {
		writeAPIErr(w, http.StatusBadRequest, "body must be {\"ttl\": <seconds>}")
		return
	}
	v := *body.TTL
	if v < 0 {
		writeAPIErr(w, http.StatusBadRequest, "ttl must be >= 0")
		return
	}
	exists := false
	s.mu.RLock()
	for _, entry := range s.cfg.Entries {
		if entry.ID == id {
			exists = true
			break
		}
	}
	s.mu.RUnlock()
	if !exists {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	// Route through commitEntries so the TTL is persisted, the registry rebuilt,
	// and the reaper/scheduler observe it under s.mu. setTTL previously wrote the
	// shared *config.Entry pointer without a lock, racing GET /api/entries, and
	// never saved the change to disk (lost across restarts).
	if err := s.commitEntries(func(list []*config.Entry) ([]*config.Entry, error) {
		for _, x := range list {
			if x.ID == id {
				x.TTL = &v
			}
		}
		return list, nil
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ttl": v})
}

func (s *Server) logTail(w http.ResponseWriter, r *http.Request, id string) {
	rt := s.sup.Get(id)
	if rt == nil {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if tail <= 0 {
		tail = 200
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "lines": rt.Logs().Tail(tail)})
}

func (s *Server) logStream(w http.ResponseWriter, r *http.Request, id string) {
	rt := s.sup.Get(id)
	if rt == nil {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	idx := rt.Logs().Total()
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lines := rt.Logs().Since(idx)
			idx += uint64(len(lines))
			for _, l := range lines {
				fmt.Fprintf(w, "data: %s\n\n", sanitizeSSE(l))
			}
			flusher.Flush()
		}
	}
}

func sanitizeSSE(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// ---- P1: discovery, presets, and entry management ----

type createReq struct {
	CloneOf   string   `json:"clone_of"`
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Aliases   []string `json:"aliases"`
	Script    string   `json:"script"`
	Args      []string `json:"args"`
	Env       []string `json:"env"`
	Port      int      `json:"port"`
	Host      string   `json:"host"`
	TTL       *int     `json:"ttl"`
	Placement string   `json:"placement"`
	Tags      []string `json:"tags"`
}

func (s *Server) scanHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIErr(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	dirs := r.URL.Query()["dir"]
	if len(dirs) == 0 {
		if d := r.URL.Query().Get("dirs"); d != "" {
			dirs = strings.Split(d, ",")
		}
	}
	if len(dirs) == 0 {
		dirs = scan.DefaultDirs()
	}
	cands, err := scan.ScanDirs(dirs)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.RLock()
	have := map[string]bool{}
	for _, e := range s.cfg.Entries {
		have[e.Script] = true
	}
	s.mu.RUnlock()

	type item struct {
		scan.Candidate
		Configured bool `json:"configured"`
	}
	items := make([]item, 0, len(cands))
	for _, c := range cands {
		items = append(items, item{Candidate: c, Configured: have[c.Script]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"dirs": dirs, "candidates": items})
}

func (s *Server) preset(w http.ResponseWriter, r *http.Request, id string) {
	e, ok := s.reg.ByID(id)
	if !ok {
		writeAPIErr(w, http.StatusNotFound, "unknown entry "+id)
		return
	}
	c, err := scan.ParseScript(e.Script)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "cannot parse script: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"script": e.Script,
		"preset": c,
	})
}

func (s *Server) createEntry(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	var ne *config.Entry
	if req.CloneOf != "" {
		base, ok := s.reg.ByID(req.CloneOf)
		if !ok {
			writeAPIErr(w, http.StatusNotFound, "unknown clone_of "+req.CloneOf)
			return
		}
		ne = base.Clone()
		ne.Aliases = nil
		ne.Pinned = false
		if req.ID != "" {
			ne.ID = req.ID
		} else {
			ne.ID = req.CloneOf + "-copy"
		}
		if req.Name != "" {
			ne.Name = req.Name
		} else if ne.Name != "" {
			ne.Name += " (copy)"
		}
	} else {
		if req.ID == "" {
			writeAPIErr(w, http.StatusBadRequest, "id is required")
			return
		}
		if req.Script == "" {
			writeAPIErr(w, http.StatusBadRequest, "script is required")
			return
		}
		if req.Port == 0 {
			writeAPIErr(w, http.StatusBadRequest, "port is required (edit the script's port into the entry)")
			return
		}
		ne = &config.Entry{
			ID:        req.ID,
			Name:      req.Name,
			Aliases:   req.Aliases,
			Script:    req.Script,
			Args:      req.Args,
			Env:       req.Env,
			Port:      req.Port,
			Host:      req.Host,
			TTL:       req.TTL,
			Placement: req.Placement,
			Tags:      req.Tags,
			Ready:     config.Ready{Type: "http", Path: "/health", Timeout: 300},
		}
	}

	err := s.commitEntries(func(list []*config.Entry) ([]*config.Entry, error) {
		for _, e := range list {
			if e.ID == ne.ID {
				return nil, fmt.Errorf("entry id %q already exists", ne.ID)
			}
		}
		return append(list, ne), nil
	})
	if err != nil {
		writeAPIErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "created", "id": ne.ID})
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.ID == "" && body.Name == "" {
		writeAPIErr(w, http.StatusBadRequest, "provide id and/or name")
		return
	}
	newID := id
	err := s.commitEntries(func(list []*config.Entry) ([]*config.Entry, error) {
		for i, e := range list {
			if e.ID != id {
				continue
			}
			ne := e.Clone()
			if body.ID != "" && body.ID != e.ID {
				for _, o := range list {
					if o.ID == body.ID {
						return nil, fmt.Errorf("entry id %q already exists", body.ID)
					}
				}
				if !containsStr(ne.Aliases, e.ID) {
					ne.Aliases = append(ne.Aliases, e.ID)
				}
				ne.ID = body.ID
				newID = body.ID
			}
			if body.Name != "" {
				ne.Name = body.Name
			}
			next := append([]*config.Entry(nil), list...)
			next[i] = ne
			return next, nil
		}
		return nil, fmt.Errorf("unknown entry %q", id)
	})
	if err != nil {
		writeAPIErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "renamed", "from": id, "id": newID})
}

func (s *Server) deleteEntry(w http.ResponseWriter, r *http.Request, id string) {
	err := s.commitEntries(func(list []*config.Entry) ([]*config.Entry, error) {
		idx := -1
		for i, e := range list {
			if e.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("unknown entry %q", id)
		}
		trashDir := filepath.Join(s.cfg.Server.StateDir, "trash")
		name := time.Now().Format("20060102-150405") + "-" + id + ".yaml"
		if err := config.WriteEntryFile(filepath.Join(trashDir, name), list[idx]); err != nil {
			return nil, fmt.Errorf("trashing entry: %w", err)
		}
		next := append([]*config.Entry(nil), list[:idx]...)
		next = append(next, list[idx+1:]...)
		return next, nil
	})
	if err != nil {
		writeAPIErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "trashed", "id": id})
}

// commitEntries applies a mutation to the entry list, validates, persists, and
// reloads the registry + supervisor. The mutation receives a copy of the slice.
func (s *Server) commitEntries(mutate func([]*config.Entry) ([]*config.Entry, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next, err := mutate(append([]*config.Entry(nil), s.cfg.Entries...))
	if err != nil {
		return err
	}
	saved := s.cfg.Entries
	s.cfg.Entries = next
	s.cfg.Normalize()
	if err := s.cfg.Validate(); err != nil {
		s.cfg.Entries = saved
		return err
	}
	if err := config.Save(s.cfg.Path, s.cfg); err != nil {
		s.cfg.Entries = saved
		return err
	}
	s.reg.Rebuild(s.cfg)
	s.sup.SyncRuntimes(s.cfg.Entries)
	return nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg}})
}
