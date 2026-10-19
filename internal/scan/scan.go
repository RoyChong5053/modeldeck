// Package scan discovers existing model-launch scripts and turns them into
// draft entries. It never modifies the scripts; it only reads and summarises.
package scan

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"modeldeck/internal/config"
)

// Candidate is one discovered script, with a ready-to-use draft entry.
type Candidate struct {
	Script    string        `json:"script"`
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Port      int           `json:"port"`
	Host      string        `json:"host"`
	Binary    string        `json:"binary"`
	Model     string        `json:"model"`
	Placement string        `json:"placement"`
	Ready     string        `json:"ready"`
	Flags     []string      `json:"flags"`
	Warnings  []string      `json:"warnings,omitempty"`
	Exists    bool          `json:"script_exists"`
	Entry     *config.Entry `json:"entry"`
}

// DefaultDirs returns the usual script locations that exist on this machine.
func DefaultDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	all := []string{
		filepath.Join(home, "LLM"),
		filepath.Join(home, "scripts", "LLM"),
		filepath.Join(home, "scripts", "LLM", "GPU"),
		filepath.Join(home, "scripts", "LLM", "CPU"),
	}
	var out []string
	for _, d := range all {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// ScanDirs walks dirs for *.sh files and parses each.
func ScanDirs(dirs []string) ([]Candidate, error) {
	seen := map[string]bool{}
	var files []string
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		root, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && (strings.HasPrefix(name, ".") || name == "old" || name == "node_modules") {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(name), ".sh") {
				return nil
			}
			abs, _ := filepath.Abs(path)
			if !seen[abs] {
				seen[abs] = true
				files = append(files, abs)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)

	cands := make([]Candidate, 0, len(files))
	for _, f := range files {
		c, err := ParseScript(f)
		if err != nil {
			continue
		}
		cands = append(cands, *c)
	}
	return cands, nil
}

var (
	reAssign   = regexp.MustCompile(`(?m)^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	reExec     = regexp.MustCompile(`(?m)^\s*exec\s+("[^"]+"|'[^']+'|\S+)`)
	reExecEnv  = regexp.MustCompile(`(?m)^\s*exec\s+env\s+("[^"]+"|'[^']+'|\S+)`)
	rePortLong = regexp.MustCompile(`--port[= ](\d{2,5})`)
	rePortJSON = regexp.MustCompile(`"port"\s*:\s*(\d{2,5})`)
	reHostLong = regexp.MustCompile(`--host[= ]("?[0-9A-Za-z.:\[\]-]+"?)`)
	reHostJSON = regexp.MustCompile(`"host"\s*:\s*"([^"]+)"`)
	reModel    = regexp.MustCompile(`(?:^|\s)(?:-m|--model)[= ]("[^"]+"|'[^']+'|[^\s]+)`)
	reNGL      = regexp.MustCompile(`(?:-ngl|--n-gpu-layers|--ngl)[= ](-?\d+)`)
	reCtx      = regexp.MustCompile(`(?:-c|--ctx-size)[= ](\d+)`)
	reDevice   = regexp.MustCompile(`--device[= ]("[^"]+"|\S+)`)
)

var knownBinaries = []string{
	"llama-server", "llama-cli", "llama-embedding", "whisper-server",
	"audiocpp_server", "speech", "python3", "python", "node", "uvicorn",
}

// ParseScript reads one shell script and extracts a draft entry.
func ParseScript(path string) (*Candidate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(raw)

	vars := map[string]string{}
	for _, m := range reAssign.FindAllStringSubmatch(text, -1) {
		vars[m[1]] = unquote(strings.TrimSpace(m[2]))
	}
	expanded := expandVars(text, vars)

	c := &Candidate{
		Script: path,
		Exists: true,
	}

	// Binary
	if m := reExecEnv.FindStringSubmatch(expanded); m != nil {
		c.Binary = unquote(m[1])
	} else if m := reExec.FindStringSubmatch(expanded); m != nil {
		c.Binary = unquote(m[1])
	}
	if c.Binary == "" {
		for _, b := range knownBinaries {
			if strings.Contains(expanded, b) {
				c.Binary = b
				break
			}
		}
	}

	// Port / host
	if m := rePortLong.FindStringSubmatch(expanded); m != nil {
		c.Port, _ = strconv.Atoi(m[1])
	} else if m := rePortJSON.FindStringSubmatch(expanded); m != nil {
		c.Port, _ = strconv.Atoi(m[1])
	}
	if m := reHostLong.FindStringSubmatch(expanded); m != nil {
		c.Host = unquote(m[1])
	} else if m := reHostJSON.FindStringSubmatch(expanded); m != nil {
		c.Host = m[1]
	}
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}

	// Model
	if m := reModel.FindStringSubmatch(expanded); m != nil {
		c.Model = unquote(m[1])
	}

	// Notable flags
	flagDefs := []struct{ flag, re string }{
		{"--embeddings", `--embeddings`},
		{"--rerank", `--rerank(?:ing)?`},
		{"--jinja", `--jinja`},
		{"--mmproj", `--mmproj`},
		{"--pooling", `--pooling[= ]\S+`},
		{"--parallel", `--parallel[= ]\d+`},
		{"--flash-attn", `--flash-attn`},
	}
	for _, fd := range flagDefs {
		if regexp.MustCompile(fd.re).MatchString(expanded) {
			c.Flags = append(c.Flags, fd.flag)
		}
	}
	if m := reNGL.FindStringSubmatch(expanded); m != nil {
		c.Flags = append(c.Flags, "-ngl "+m[1])
	}
	if m := reCtx.FindStringSubmatch(expanded); m != nil {
		c.Flags = append(c.Flags, "-c "+m[1])
	}
	if m := reDevice.FindStringSubmatch(expanded); m != nil {
		c.Flags = append(c.Flags, "--device "+unquote(m[1]))
	}

	c.Placement = guessPlacement(path, expanded, c.Flags)
	c.Ready = "http"

	// Identity
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	c.ID = slug(base)
	c.Name = humanize(base)

	// Readiness
	if c.Port == 0 {
		c.Ready = "none"
		c.Warnings = append(c.Warnings, "no --port found; set a port and ready probe manually")
	}
	if c.Binary == "" {
		c.Warnings = append(c.Warnings, "could not identify the launched binary")
	}
	if c.Model != "" && filepath.IsAbs(c.Model) {
		if _, err := os.Stat(c.Model); err != nil {
			c.Warnings = append(c.Warnings, "model file not found: "+c.Model)
		}
	}

	c.Entry = c.buildEntry()
	return c, nil
}

func (c *Candidate) buildEntry() *config.Entry {
	e := &config.Entry{
		ID:        c.ID,
		Name:      c.Name,
		Script:    c.Script,
		Port:      c.Port,
		Placement: c.Placement,
		Tags:      []string{"scanned"},
	}
	if c.Host != "" && c.Host != "0.0.0.0" {
		e.Host = c.Host
	}
	e.Ready = config.Ready{Type: c.Ready, Timeout: 300}
	if c.Ready == "http" {
		e.Ready.Path = "/health"
	}
	return e
}

func guessPlacement(path, text string, flags []string) string {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "cpu") {
		return "cpu"
	}
	for _, f := range flags {
		if f == "-ngl 0" {
			return "cpu"
		}
		if strings.HasPrefix(f, "--device") && strings.Contains(f, "cpu") {
			return "cpu"
		}
	}
	if strings.Contains(strings.ToLower(text), "build_cpu") {
		return "cpu"
	}
	return "gpu"
}

func expandVars(text string, vars map[string]string) string {
	// ${VAR} form first, then $VAR.
	reBrace := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	text = reBrace.ReplaceAllStringFunc(text, func(s string) string {
		name := s[2 : len(s)-1]
		if v, ok := vars[name]; ok {
			return v
		}
		return s
	})
	rePlain := regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	text = rePlain.ReplaceAllStringFunc(text, func(s string) string {
		name := s[1:]
		if v, ok := vars[name]; ok {
			return v
		}
		return s
	})
	return text
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func humanize(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
