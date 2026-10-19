package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"modeldeck/internal/config"
	"modeldeck/internal/httpapi"
	"modeldeck/internal/proxy"
	"modeldeck/internal/registry"
	"modeldeck/internal/runtime"
	"modeldeck/internal/sched"
	"modeldeck/internal/web"
)

const version = "0.1.0"

func main() {
	args := os.Args[1:]
	cfgPath := defaultConfigPath()
	keep := false
	var cmd string
	var rest []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-config" || a == "--config" || a == "-c":
			if i+1 < len(args) {
				cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-config="):
			cfgPath = strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case a == "-keep":
			keep = true
		case a == "-h" || a == "--help" || a == "help":
			usage()
			return
		case cmd == "":
			cmd = a
		default:
			rest = append(rest, a)
		}
	}

	switch cmd {
	case "":
		usage()
		os.Exit(2)
	case "serve":
		runServe(cfgPath, keep)
	case "validate":
		runValidate(cfgPath)
	case "ls":
		runLS(cfgPath)
	case "start", "stop", "restart":
		runControl(cfgPath, cmd, rest)
	case "env":
		runEnv(cfgPath, rest)
	case "version":
		fmt.Printf("modeldeck deck v%s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`modeldeck — script-first local model scheduler

Usage:
  deck serve    [-config PATH] [-keep]   run the scheduler + proxy + WebUI
  deck validate [-config PATH]           parse and validate the config
  deck ls       [-config PATH]           list entries and live state
  deck start|stop|restart <id>           control one entry on a running deck
  deck env      <id>                     print client env for one entry
  deck version

Default config: ~/.config/modeldeck/entries.yaml (env MODELDECK_CONFIG overrides)
`)
}

func defaultConfigPath() string {
	if p := os.Getenv("MODELDECK_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "modeldeck", "entries.yaml")
}

func runServe(cfgPath string, keep bool) {
	cfg, err := config.Load(cfgPath)
	fatal(err)

	sup, err := runtime.NewSupervisor(cfg)
	fatal(err)
	reg := registry.New(cfg)
	sch := sched.New(cfg, sup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sch.Run(ctx)

	pr := proxy.New(cfg, reg, sch)
	api := httpapi.New(cfg, reg, sup, sch, pr, web.Handler())

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           api,
		ReadHeaderTimeout: 30 * time.Second,
	}

	fmt.Printf("modeldeck v%s listening on http://%s\n", version, cfg.Server.Listen)
	fmt.Printf("  config : %s\n", cfgPath)
	fmt.Printf("  state  : %s\n", cfg.Server.StateDir)
	fmt.Printf("  entries: %d\n", len(cfg.Entries))

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	case s := <-sig:
		fmt.Printf("\nreceived %s, shutting down…\n", s)
	}

	cancel()
	shutdownCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
	defer c()
	_ = srv.Shutdown(shutdownCtx)

	if !keep {
		for _, rt := range sup.List() {
			if st := rt.State(); st == runtime.StateReady || st == runtime.StateStarting {
				fmt.Printf("stopping %s…\n", rt.Entry.ID)
				_ = sup.Stop(rt.Entry.ID)
			}
		}
	}
}

func runValidate(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "INVALID:", err)
		os.Exit(1)
	}
	fmt.Printf("OK  %s\n", cfgPath)
	fmt.Printf("    entries=%d  listen=%s  state_dir=%s\n", len(cfg.Entries), cfg.Server.Listen, cfg.Server.StateDir)
	warn := 0
	for _, e := range cfg.Entries {
		mark := "✓"
		if _, err := os.Stat(e.Script); err != nil {
			mark = "!"
			warn++
		}
		fmt.Printf("    %s %-24s port=%-6d ttl=%-6d script=%s\n",
			mark, e.ID, e.Port, e.TTLSeconds(cfg.Budgets.TTLDefaultS), e.Script)
	}
	if warn > 0 {
		fmt.Printf("    %d script(s) not found on disk\n", warn)
	}
}

func runLS(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	fatal(err)
	base := controlBase(cfg)
	body, err := httpDo(cfg, http.MethodGet, base+"/api/status", nil)
	fatal(err)

	var payload struct {
		Entries []runtime.Snapshot `json:"entries"`
		Mem     struct {
			AvailableMB int64 `json:"available_mb"`
			TotalMB     int64 `json:"total_mb"`
		} `json:"mem"`
		GPU struct {
			OK      bool  `json:"ok"`
			FreeMB  int64 `json:"free_mb"`
			TotalMB int64 `json:"total_mb"`
		} `json:"gpu"`
	}
	fatal(json.Unmarshal(body, &payload))

	fmt.Printf("RAM available %d/%d MB", payload.Mem.AvailableMB, payload.Mem.TotalMB)
	if payload.GPU.OK {
		fmt.Printf("   GPU free %d/%d MB", payload.GPU.FreeMB, payload.GPU.TotalMB)
	}
	fmt.Println()
	fmt.Printf("%-24s %-9s %-6s %-8s %-10s %-9s %s\n", "ID", "STATE", "PORT", "INFLIGHT", "RSS", "VRAM", "ERROR")
	for _, e := range payload.Entries {
		rss := "—"
		if e.CurRSSKB > 0 {
			rss = fmt.Sprintf("%dMB", e.CurRSSKB/1024)
		}
		vram := "—"
		if e.CurVRAMMB > 0 {
			vram = fmt.Sprintf("%dMB", e.CurVRAMMB)
		}
		fmt.Printf("%-24s %-9s %-6d %-8d %-10s %-9s %s\n",
			e.ID, e.State, e.Port, e.Inflight, rss, vram, e.LastErr)
	}
}

func runControl(cfgPath, action string, rest []string) {
	if len(rest) < 1 {
		fmt.Fprintf(os.Stderr, "usage: deck %s <id>\n", action)
		os.Exit(2)
	}
	id := rest[0]
	cfg, err := config.Load(cfgPath)
	fatal(err)
	base := controlBase(cfg)
	url := fmt.Sprintf("%s/api/entries/%s/%s", base, id, action)
	body, err := httpDo(cfg, http.MethodPost, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("%s %s: %s\n", action, id, strings.TrimSpace(string(body)))
}

func runEnv(cfgPath string, rest []string) {
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: deck env <id>")
		os.Exit(2)
	}
	cfg, err := config.Load(cfgPath)
	fatal(err)
	id := rest[0]
	reg := registry.New(cfg)
	e, ok := reg.ByID(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown entry %q\n", id)
		os.Exit(1)
	}
	base := controlBase(cfg)
	fmt.Printf("export OPENAI_BASE_URL=%s/v1\n", base)
	fmt.Printf("export OPENAI_API_KEY=modeldeck\n")
	fmt.Printf("export OPENAI_MODEL=%s\n", e.ID)
	fmt.Println()
	fmt.Printf(`curl -s %s/v1/chat/completions -H 'Content-Type: application/json' \`+"\n", base)
	fmt.Printf("  -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n", e.ID)
	fmt.Println()
	fmt.Printf("generic passthrough: %s/run/%s/<path>\n", base, e.ID)
}

func controlBase(cfg *config.Config) string {
	host := cfg.Server.Listen
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	port := "12688"
	if i := strings.LastIndex(cfg.Server.Listen, ":"); i >= 0 {
		port = cfg.Server.Listen[i+1:]
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

func httpDo(cfg *config.Config, method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if cfg.Server.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Server.Token)
	}
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w (is deck serve running?)", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return b, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
