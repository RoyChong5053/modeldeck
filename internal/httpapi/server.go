package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"modeldeck/internal/config"
	"modeldeck/internal/proxy"
	"modeldeck/internal/registry"
	"modeldeck/internal/runtime"
	"modeldeck/internal/sched"
)

// Server wires the control plane, the data plane, and the WebUI onto one port.
type Server struct {
	cfg   *config.Config
	reg   *registry.Registry
	sup   *runtime.Supervisor
	sch   *sched.Scheduler
	proxy *proxy.Service
	web   http.Handler
}

func New(cfg *config.Config, reg *registry.Registry, sup *runtime.Supervisor, sch *sched.Scheduler, pr *proxy.Service, web http.Handler) *Server {
	return &Server{cfg: cfg, reg: reg, sup: sup, sch: sch, proxy: pr, web: web}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "unauthorized"}})
		return
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

func (s *Server) authorized(r *http.Request) bool {
	token := s.cfg.Server.Token
	if token == "" {
		return true
	}
	p := r.URL.Path
	if p == "/health" || p == "/" || strings.HasPrefix(p, "/static/") {
		return true
	}
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == token {
		return true
	}
	return r.URL.Query().Get("token") == token
}

func (s *Server) control(w http.ResponseWriter, r *http.Request, rel string) {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	switch parts[0] {
	case "status":
		writeJSON(w, http.StatusOK, s.statusPayload())
	case "mem":
		writeJSON(w, http.StatusOK, s.memPayload())
	case "entries":
		if len(parts) == 1 {
			writeJSON(w, http.StatusOK, map[string]any{"entries": s.cfg.Entries})
			return
		}
		id := parts[1]
		if len(parts) == 2 {
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
		case "ttl":
			s.setTTL(w, r, id)
		case "start", "stop", "restart":
			s.action(w, r, id, parts[2])
		default:
			writeAPIErr(w, http.StatusNotFound, "unknown action "+parts[2])
		}
	default:
		writeAPIErr(w, http.StatusNotFound, "unknown endpoint /api/"+rel)
	}
}

func (s *Server) statusPayload() map[string]any {
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
		"vram_reserve_mb": s.cfg.Budgets.VRAMReserveMB,
		"ram_reserve_mb":  s.cfg.Budgets.RAMReserveMB,
		"max_starting":    s.cfg.Budgets.MaxStarting,
		"on_no_fit":       s.cfg.Budgets.OnNoFit,
		"queue_timeout":   s.cfg.Budgets.QueueTimeoutS,
		"ttl_default":     s.cfg.Budgets.TTLDefaultS,
	}
	payload["listen"] = s.cfg.Server.Listen
	return payload
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
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Pinned != nil {
		e.Pinned = *body.Pinned
	} else {
		e.Pinned = !e.Pinned
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "pinned": e.Pinned})
}

func (s *Server) setTTL(w http.ResponseWriter, r *http.Request, id string) {
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
	e.TTL = &v
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg}})
}
