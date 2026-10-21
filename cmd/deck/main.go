package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	"modeldeck/internal/scan"
	"modeldeck/internal/sched"
	"modeldeck/internal/version"
	"modeldeck/internal/web"
)

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
	case "init":
		runInit(cfgPath, rest)
	case "scan":
		runScan(cfgPath, rest)
	case "preset":
		runPreset(cfgPath, rest)
	case "start", "stop", "restart":
		runControl(cfgPath, cmd, rest)
	case "env":
		runEnv(cfgPath, rest)
	case "token":
		runToken(cfgPath, rest)
	case "version":
		fmt.Printf("modeldeck deck v%s\n", version.Version)
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
  deck init     [-dir A,B] [-force]      create a config by scanning your scripts
  deck validate [-config PATH]           parse and validate the config
  deck ls       [-config PATH]           list entries and live state
  deck scan     [-dir A,B] [-o FILE | -merge FILE] [-force]
                                         discover scripts and draft entries
  deck preset   <id>                     show an entry's parsed script preset
  deck start|stop|restart <id>           control one entry on a running deck
  deck env      <id>                     print client env for one entry
  deck token                              print the access token + one-api channel snippet
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

// uniquifyEntries makes entry ids unique, preferring a parent-directory hint
// (e.g. "-gpu"/"-cpu") so entries from different dirs stay recognisable.
func uniquifyEntries(entries []*config.Entry) {
	used := map[string]bool{}
	validHint := func(s string) bool {
		if s == "" || len(s) > 12 {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
		return true
	}
	for _, e := range entries {
		if !used[e.ID] {
			used[e.ID] = true
			continue
		}
		hint := strings.ToLower(filepath.Base(filepath.Dir(e.Script)))
		if validHint(hint) && !used[e.ID+"-"+hint] {
			e.ID = e.ID + "-" + hint
		} else {
			for i := 2; ; i++ {
				cand := fmt.Sprintf("%s-%d", e.ID, i)
				if !used[cand] {
					e.ID = cand
					break
				}
			}
		}
		used[e.ID] = true
	}
}

// loadConfig reads the config, printing setup hints when the file is missing.
func loadConfig(cfgPath string) *config.Config {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "config not found: %s\n\n", cfgPath)
			fmt.Fprintln(os.Stderr, "create it with one of:")
			fmt.Fprintf(os.Stderr, "  deck init                    # scan your script dirs and write it\n")
			fmt.Fprintf(os.Stderr, "  deck scan -o %q\n", cfgPath)
			fmt.Fprintf(os.Stderr, "  cp examples/entries.example.yaml %q\n", cfgPath)
			os.Exit(1)
		}
		fatal(err)
	}
	return cfg
}

// runInit writes a starter config: entries discovered from your script dirs,
// or the bundled example when nothing is found.
func runInit(cfgPath string, rest []string) {
	var dirs []string
	force := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-dir" || a == "--dir":
			if i+1 < len(rest) {
				dirs = append(dirs, strings.Split(rest[i+1], ",")...)
				i++
			}
		case strings.HasPrefix(a, "-dir="), strings.HasPrefix(a, "--dir="):
			dirs = append(dirs, strings.Split(a[strings.Index(a, "=")+1:], ",")...)
		case a == "-force" || a == "--force":
			force = true
		default:
			if !strings.HasPrefix(a, "-") {
				dirs = append(dirs, a)
			}
		}
	}
	if _, err := os.Stat(cfgPath); err == nil && !force {
		fmt.Fprintf(os.Stderr, "config already exists: %s (use -force to overwrite)\n", cfgPath)
		os.Exit(1)
	}
	if len(dirs) == 0 {
		dirs = scan.DefaultDirs()
	}

	cands, err := scan.ScanDirs(dirs)
	fatal(err)
	var entries []*config.Entry
	for _, c := range cands {
		if c.Entry != nil && c.Entry.Port > 0 {
			entries = append(entries, c.Entry)
		}
	}
	uniquifyEntries(entries)

	if len(entries) == 0 {
		if ex, err := os.ReadFile("examples/entries.example.yaml"); err == nil {
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err == nil {
				if err := os.WriteFile(cfgPath, ex, 0o644); err == nil {
					fmt.Printf("no scripts found in %s\n", strings.Join(dirs, ", "))
					fmt.Printf("copied examples/entries.example.yaml → %s\n", cfgPath)
					fmt.Println("edit it to match this machine, then run: ./start.sh")
					return
				}
			}
		}
		fmt.Printf("no scripts found in %s; writing a config with no entries\n", strings.Join(dirs, ", "))
	}

	cfg := &config.Config{
		Version: 1,
		Server:  config.Server{Listen: "127.0.0.1:12688", StateDir: defaultStateDir()},
		Budgets: config.Budgets{
			VRAMReserveMB: 800, RAMReserveMB: 2500, MaxStarting: 2,
			OnNoFit: "queue", QueueTimeoutS: 180, TTLDefaultS: 300,
		},
		Entries: entries,
	}
	cfg.Normalize()
	fatal(cfg.Validate())
	fatal(config.Save(cfgPath, cfg))

	fmt.Printf("wrote %s\n", cfgPath)
	fmt.Printf("  %d entries from: %s\n", len(entries), strings.Join(dirs, ", "))
	fmt.Println()
	fmt.Println("next:")
	fmt.Println("  ./deck validate          # sanity check")
	fmt.Println("  ./start.sh               # then open http://127.0.0.1:12688")
	fmt.Println()
	fmt.Println("note: entries start with unmeasured memory; deck serializes two unmeasured")
	fmt.Println("      GPU models until their first runs record peaks. Tune budgets.* if needed.")
}

func runServe(cfgPath string, keep bool) {
	cfg := loadConfig(cfgPath)

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
		Handler:           api,
		ReadHeaderTimeout: 30 * time.Second,
	}

	fmt.Printf("modeldeck v%s listening on http://%s\n", version.Version, cfg.Server.Listen)
	fmt.Printf("  config : %s\n", cfgPath)
	fmt.Printf("  state  : %s\n", cfg.Server.StateDir)
	fmt.Printf("  entries: %d\n", len(cfg.Entries))
	for _, u := range webURLs(cfg, cfg.Server.Token) {
		fmt.Printf("  WebUI  : %s\n", u)
	}

	errCh := make(chan error, 1)
	go func() {
		lns, err := listenAll(cfg.Server.Listen)
		if err != nil {
			errCh <- err
			return
		}
		for _, ln := range lns {
			go func(ln net.Listener) {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					errCh <- err
				}
			}(ln)
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

// listenAll binds every address in a comma separated list. Addresses that
// cannot be bound (a LAN cable that is unplugged, an address that is gone)
// are skipped with a warning as long as at least one listener comes up.
func listenAll(list string) ([]net.Listener, error) {
	var out []net.Listener
	var lastErr error
	for _, addr := range listenAddrs(list) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			out = append(out, ln)
			continue
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "listen %s failed: %v\n", addr, err)
	}
	if len(out) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no listen address configured")
		}
		return nil, lastErr
	}
	return out, nil
}

// listenAddrs splits a listen list into addresses.
func listenAddrs(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// webURLs returns the URLs a human should open, token included when set.
//
// A wildcard bind (0.0.0.0) is expanded into one URL per local address, so
// the printed links work from a phone on the LAN and over Tailscale.
func webURLs(cfg *config.Config, token string) []string {
	return runtime.ListenURLs(cfg.Server.Listen, token)
}

// lanBase picks the address a *different* machine should use to reach this
// deck: a routable local address, never the loopback.
func lanBase(cfg *config.Config) string {
	for _, addr := range listenAddrs(cfg.Server.Listen) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		switch host {
		case "0.0.0.0", "::", "[::]", "":
			if ips := runtime.LocalIPs(); len(ips) > 0 {
				return "http://" + ips[0] + ":" + port
			}
			continue
		case "127.0.0.1", "localhost", "::1":
			continue
		}
		return "http://" + host + ":" + port
	}
	return controlBase(cfg)
}

func runValidate(cfgPath string) {
	cfg := loadConfig(cfgPath)
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
	cfg := loadConfig(cfgPath)
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
	cfg := loadConfig(cfgPath)
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
	cfg := loadConfig(cfgPath)
	id := rest[0]
	reg := registry.New(cfg)
	e, ok := reg.ByID(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown entry %q\n", id)
		os.Exit(1)
	}
	base := controlBase(cfg)
	fmt.Printf("export OPENAI_BASE_URL=%s/v1\n", base)
	key := cfg.Server.Token
	if key == "" {
		key = "modeldeck"
	}
	fmt.Printf("export OPENAI_API_KEY=%s\n", key)
	fmt.Printf("export OPENAI_MODEL=%s\n", e.ID)
	fmt.Println()
	fmt.Printf(`curl -s %s/v1/chat/completions -H 'Content-Type: application/json' \`+"\n", base)
	fmt.Printf("  -d '{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n", e.ID)
	if cfg.Server.Token != "" {
		fmt.Println()
		fmt.Printf("auth: -H 'Authorization: Bearer %s'\n", cfg.Server.Token)
	}
	fmt.Println()
	fmt.Printf("generic passthrough: %s/run/%s/<path>\n", base, e.ID)
}

// runToken prints everything needed to point a third-party client (one-api,
// openai-compatible gateways, …) at this deck.
//
//	deck token           show the token and a ready-to-paste one-api channel
//	deck token --write   also make sure <project>/.env holds the token
func runToken(cfgPath string, rest []string) {
	cfg := loadConfig(cfgPath)
	write := false
	for _, a := range rest {
		switch a {
		case "--write", "-w":
			write = true
		}
	}

	tok, src := cfg.Server.Token, cfg.Server.TokenSource
	envPath := ""
	realTok := tok
	if write {
		var err error
		tok, envPath, err = ensureEnvFile(cfg)
		if err != nil {
			fatal(err)
		}
		realTok, src = tok, ".env"
	} else if tok == "" {
		// Fall back to the project folder's .env (the per-device source of
		// truth) so the token is visible without exporting anything.
		if t, p := config.EnvFileToken(); t != "" {
			tok, realTok, src, envPath = t, t, ".env", p
		}
	}
	if tok == "" {
		tok = "(未设置令牌：任何人都能访问 /api /v1 /run)"
	}

	fmt.Printf("token      : %s\n", tok)
	if src != "" {
		fmt.Printf("来源       : %s\n", src)
	}
	if envPath != "" {
		fmt.Printf("令牌文件   : %s\n", envPath)
	}
	fmt.Printf("本机 base   : %s\n", controlBase(cfg))
	lan := lanBase(cfg)
	fmt.Printf("局域网 base : %s   ← 填到 one-api 渠道\n", lan)
	for _, u := range webURLs(cfg, realTok) {
		fmt.Printf("webui      : %s\n", u)
	}
	fmt.Println()
	fmt.Println("one-api 渠道（新建渠道时填这些）:")
	fmt.Printf("  类型     : OpenAI\n")
	fmt.Printf("  Base URL : %s\n", lan)
	fmt.Printf("  密钥     : %s\n", realTok)
	fmt.Printf("  模型     : %s\n", modelNames(cfg))
	fmt.Println()
	fmt.Println("客户端直连（在本机跑时）:")
	fmt.Printf("  OPENAI_BASE_URL=%s/v1\n", controlBase(cfg))
	fmt.Printf("  OPENAI_API_KEY=%s\n", realTok)
}

// ensureEnvFile makes sure the project folder carries a .env with the access
// token. That file is the single source of truth per device: copy it to a new
// device and the token travels along. The deployment layer feeds it into the
// environment (systemd EnvironmentFile=, start-termux.sh export).
func ensureEnvFile(cfg *config.Config) (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(filepath.Dir(exe), ".env")
	if t, p := config.EnvFileToken(); t != "" {
		return t, p, nil
	}
	tok := cfg.Server.Token
	if tok == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return "", "", err
		}
		tok = "deck-" + hex.EncodeToString(buf)
	}
	content := "# modeldeck access token — this device\n" +
		"# 复制这个文件到新设备即可复用同一令牌；one-api 渠道的「密钥」填 DECK_TOKEN 的值。\n" +
		"DECK_TOKEN=" + tok + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", "", err
	}
	return tok, path, nil
}

// modelNames lists the model names the deck answers to.
func modelNames(cfg *config.Config) string {
	var names []string
	for _, e := range cfg.Entries {
		if e.Mode == "job" {
			continue
		}
		names = append(names, e.ID)
	}
	if len(names) == 0 {
		return "(无)"
	}
	if len(names) > 12 {
		return strings.Join(names[:12], ", ") + fmt.Sprintf(" … 共 %d 个", len(names))
	}
	return strings.Join(names, ", ")
}

func runScan(cfgPath string, rest []string) {
	var dirs []string
	out, merge := "", ""
	force := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-dir" || a == "--dir":
			if i+1 < len(rest) {
				dirs = append(dirs, strings.Split(rest[i+1], ",")...)
				i++
			}
		case strings.HasPrefix(a, "-dir="):
			dirs = append(dirs, strings.Split(strings.TrimPrefix(a, "-dir="), ",")...)
		case strings.HasPrefix(a, "--dir="):
			dirs = append(dirs, strings.Split(strings.TrimPrefix(a, "--dir="), ",")...)
		case a == "-o" || a == "--out":
			if i+1 < len(rest) {
				out = rest[i+1]
				i++
			}
		case strings.HasPrefix(a, "-o="), strings.HasPrefix(a, "--out="):
			out = a[strings.Index(a, "=")+1:]
		case a == "-merge":
			if i+1 < len(rest) {
				merge = rest[i+1]
				i++
			}
		case strings.HasPrefix(a, "-merge="):
			merge = strings.TrimPrefix(a, "-merge=")
		case a == "-force":
			force = true
		default:
			if !strings.HasPrefix(a, "-") {
				dirs = append(dirs, a)
			}
		}
	}
	if len(dirs) == 0 {
		dirs = scan.DefaultDirs()
	}

	cands, err := scan.ScanDirs(dirs)
	fatal(err)
	if len(cands) == 0 {
		fmt.Println("no scripts found in", strings.Join(dirs, ", "))
		return
	}

	printScan(cands)

	entries := make([]*config.Entry, 0, len(cands))
	skipped := 0
	for _, c := range cands {
		if c.Entry == nil || c.Entry.Port == 0 {
			skipped++
			continue
		}
		entries = append(entries, c.Entry)
	}
	uniquifyEntries(entries)

	switch {
	case out != "":
		if _, err := os.Stat(out); err == nil && !force {
			fmt.Fprintf(os.Stderr, "refusing to overwrite %s (use -force)\n", out)
			os.Exit(1)
		}
		cfg := &config.Config{
			Version: 1,
			Server:  config.Server{Listen: "127.0.0.1:12688", StateDir: defaultStateDir()},
			Budgets: config.Budgets{
				VRAMReserveMB: 800, RAMReserveMB: 2500, MaxStarting: 2,
				OnNoFit: "queue", QueueTimeoutS: 180, TTLDefaultS: 300,
			},
			Entries: entries,
		}
		cfg.Normalize()
		fatal(cfg.Validate())
		fatal(config.Save(out, cfg))
		fmt.Printf("\nwrote %d entries to %s\n", len(entries), out)
	case merge != "":
		var cfg *config.Config
		if _, err := os.Stat(merge); err == nil {
			cfg, err = config.Load(merge)
			fatal(err)
		} else {
			cfg = &config.Config{Version: 1}
		}
		haveScript := map[string]bool{}
		haveID := map[string]bool{}
		for _, e := range cfg.Entries {
			haveScript[e.Script] = true
			haveID[e.ID] = true
		}
		added := 0
		for _, e := range entries {
			if haveScript[e.Script] {
				continue
			}
			e.ID = uniqueID(e.ID, haveID)
			haveID[e.ID] = true
			haveScript[e.Script] = true
			cfg.Entries = append(cfg.Entries, e)
			added++
		}
		cfg.Normalize()
		fatal(cfg.Validate())
		fatal(config.Save(merge, cfg))
		fmt.Printf("\nmerged %d new entries into %s (backup: %s.bak)\n", added, merge, merge)
	default:
		fmt.Printf("\n%d entries with a port found (%d skipped without a port).\n", len(entries), skipped)
		fmt.Println("write a new config with:  deck scan -o entries.yaml")
		fmt.Println("merge into an existing:   deck scan -merge ~/.config/modeldeck/entries.yaml")
	}
}

func printScan(cands []scan.Candidate) {
	fmt.Printf("%-26s %-6s %-5s %-9s %-6s %s\n", "ID", "PORT", "PLACE", "READY", "BINARY", "SCRIPT")
	for _, c := range cands {
		binary := c.Binary
		if binary == "" {
			binary = "?"
		}
		fmt.Printf("%-26s %-6d %-5s %-9s %-6s %s\n",
			c.ID, c.Port, c.Placement, c.Ready, binary, filepath.Base(c.Script))
		for _, warn := range c.Warnings {
			fmt.Printf("    ! %s\n", warn)
		}
	}
}

func uniqueID(id string, used map[string]bool) string {
	if !used[id] {
		return id
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", id, i)
		if !used[cand] {
			return cand
		}
	}
}

func defaultStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "modeldeck")
}

func runPreset(cfgPath string, rest []string) {
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: deck preset <id>")
		os.Exit(2)
	}
	cfg := loadConfig(cfgPath)
	reg := registry.New(cfg)
	id := rest[0]
	e, ok := reg.ByID(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown entry %q\n", id)
		os.Exit(1)
	}
	c, err := scan.ParseScript(e.Script)
	fatal(err)

	fmt.Printf("entry      : %s (%s)\n", e.ID, e.DisplayName())
	fmt.Printf("script     : %s\n", e.Script)
	fmt.Printf("binary     : %s\n", c.Binary)
	if c.Model != "" {
		fmt.Printf("model      : %s\n", c.Model)
	}
	fmt.Printf("listen     : %s:%d  (ready: %s)\n", c.Host, c.Port, c.Ready)
	fmt.Printf("placement  : %s\n", c.Placement)
	if len(c.Flags) > 0 {
		fmt.Printf("flags      : %s\n", strings.Join(c.Flags, ", "))
	}
	for _, warn := range c.Warnings {
		fmt.Printf("warning    : %s\n", warn)
	}
}

// controlBase picks the address a local CLI should talk to: a loopback
// address when one is configured, otherwise the first one.
func controlBase(cfg *config.Config) string {
	addrs := listenAddrs(cfg.Server.Listen)
	fallback := ""
	for _, addr := range addrs {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		base := fmt.Sprintf("http://%s:%s", host, port)
		if fallback == "" {
			fallback = base
		}
		if host == "127.0.0.1" || host == "localhost" || host == "::1" {
			return base
		}
	}
	if fallback == "" {
		return "http://127.0.0.1:12688"
	}
	// 0.0.0.0 is not an address a client can connect to
	if strings.HasPrefix(fallback, "http://0.0.0.0:") {
		return "http://127.0.0.1:" + strings.TrimPrefix(fallback, "http://0.0.0.0:")
	}
	return fallback
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
