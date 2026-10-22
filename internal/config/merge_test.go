package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A config file as a person would write it by hand: comments everywhere.
const handWritten = `# modeldeck — written by hand
version: 1
server:
  listen: 0.0.0.0:12688
  token: ""
  label: 主力机
  state_dir: /tmp/modeldeck-state
budgets:
  vram_reserve_mb: 800
  ram_reserve_mb: 2500
  max_starting: 2
  on_no_fit: queue
  queue_timeout: 180
  ttl_default: 300
entries:
  # 主模型：常驻，别让它 unloaded
  - id: gemma-rp
    name: Gemma rp
    script: /home/you/LLM/gemma-rp.sh
    port: 12888
    ready: { type: http, path: /health, timeout: 300 }
    placement: gpu
    tags: [scanned]

  - id: jina-rerank-cpu
    name: Jina rerank CPU
    script: /home/you/LLM/jina-rerank-CPU.sh
    port: 11437
    ready: { type: http, path: /health, timeout: 300 }
    placement: cpu
    tags: [scanned]
`

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "entries.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Saving a config that did not change must not touch the file at all: the
// comments a person wrote by hand have to survive.
func TestSaveEntriesKeepsCommentsWhenNothingChanged(t *testing.T) {
	p := writeTemp(t, handWritten)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveEntries(p, cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "# 主模型：常驻") {
		t.Fatalf("the hand-written comment was lost:\n%s", got)
	}
	if !strings.Contains(got, "# modeldeck — written by hand") {
		t.Fatalf("the comment at the top of the file was lost:\n%s", got)
	}
	if !strings.Contains(got, "id: jina-rerank-cpu") {
		t.Fatalf("an entry was lost:\n%s", got)
	}
	// The indentation of the file is kept, not forced to the deck's default.
	if !strings.Contains(got, "\n  - id: gemma-rp") {
		t.Fatalf("the indentation style was not kept:\n%s", got)
	}
	// A second save must not change the file any more: no churn.
	if err := SaveEntries(p, cfg); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(p)
	if string(b2) != got {
		t.Fatalf("the file is not stable:\n--- 1 ---\n%s\n--- 2 ---\n%s", got, string(b2))
	}
}

// Changing one entry rewrites that entry only; the other entry keeps its
// values and every comment around it stays exactly as it was.
func TestSaveEntriesOnlyTouchesTheChangedEntry(t *testing.T) {
	p := writeTemp(t, handWritten)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cfg.Entries {
		if e.ID == "gemma-rp" {
			e.Pinned = true
		}
	}
	if err := SaveEntries(p, cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	if !strings.Contains(got, "pinned: true") {
		t.Fatalf("the change was not written:\n%s", got)
	}
	if !strings.Contains(got, "# 主模型：常驻") {
		t.Fatalf("the comment above the changed entry was lost:\n%s", got)
	}

	// The untouched entry must still hold the same values.
	after, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var other *Entry
	for _, e := range after.Entries {
		if e.ID == "jina-rerank-cpu" {
			other = e
		}
	}
	if other == nil {
		t.Fatalf("the untouched entry disappeared:\n%s", got)
	}
	if other.Script != "/home/you/LLM/jina-rerank-CPU.sh" || other.Port != 11437 ||
		other.Placement != "cpu" || other.Ready.Type != "http" || other.Ready.Path != "/health" {
		t.Fatalf("the untouched entry changed:\n%+v", other)
	}
	// It kept its flow style, i.e. it was not rebuilt from the struct.
	if !strings.Contains(got, "ready: {type: http, path: /health, timeout: 300}") {
		t.Fatalf("the untouched entry was rewritten from the struct:\n%s", got)
	}
}

// A broken file on disk must not be silently overwritten.
func TestSaveEntriesRefusesBrokenFile(t *testing.T) {
	p := writeTemp(t, "entries: [\n  this is not yaml")
	cfg := &Config{Version: 1, Entries: []*Entry{{ID: "x", Script: "/bin/true", Port: 1}}}
	cfg.Normalize()
	err := SaveEntries(p, cfg)
	if err == nil {
		t.Fatal("expected an error for a broken file")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "entries: [\n  this is not yaml" {
		t.Fatalf("the broken file was overwritten:\n%s", string(b))
	}
}

// A new entry is appended, a deleted one disappears.
func TestSaveEntriesAddsAndRemoves(t *testing.T) {
	p := writeTemp(t, handWritten)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Entries = cfg.Entries[:1]
	cfg.Entries = append(cfg.Entries, &Entry{
		ID: "new-one", Name: "New", Script: "/home/you/LLM/new.sh", Port: 13000,
	})
	cfg.Normalize()
	if err := SaveEntries(p, cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if !strings.Contains(got, "id: new-one") {
		t.Fatalf("the new entry was not written:\n%s", got)
	}
	if strings.Contains(got, "jina-rerank-cpu") {
		t.Fatalf("the deleted entry is still there:\n%s", got)
	}
	if !strings.Contains(got, "# 主模型：常驻") {
		t.Fatalf("the comment was lost:\n%s", got)
	}
}

// The token that lives in the environment must not be written into the file.
func TestSaveEntriesKeepsEnvTokenOutOfTheFile(t *testing.T) {
	p := writeTemp(t, handWritten)
	t.Setenv("DECK_TOKEN", "deck-secret")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveEntries(p, cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "deck-secret") {
		t.Fatalf("the environment token leaked into the file:\n%s", string(b))
	}
}

// The editor sends the YAML as text; the comments in it must land in the file.
func TestSaveEntriesWithKeepsTheTypedComments(t *testing.T) {
	p := writeTemp(t, handWritten)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	text, err := EntryYAML(p, "gemma-rp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "id: gemma-rp") {
		t.Fatalf("the entry text is wrong:\n%s", text)
	}
	// The person types a comment and a new name.
	text = "# typed in the browser\n" + strings.Replace(text, "name: Gemma rp", "name: Gemma rp v2", 1)
	node, entry, err := ParseEntryYAML(text)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "Gemma rp v2" {
		t.Fatalf("the name was not parsed: %+v", entry)
	}
	for _, e := range cfg.Entries {
		if e.ID == "gemma-rp" {
			e.Name = entry.Name
		}
	}
	if err := SaveEntriesWith(p, cfg, map[string]*yaml.Node{"gemma-rp": node}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if !strings.Contains(got, "# typed in the browser") {
		t.Fatalf("the comment typed in the editor was lost:\n%s", got)
	}
	// and it sits above the entry, not behind the list marker
	if strings.Contains(got, "- # typed in the browser") {
		t.Fatalf("the comment ended up behind the list marker:\n%s", got)
	}
	if !strings.Contains(got, "# typed in the browser\n  # 主模型") {
		t.Fatalf("the comment is not above the entry:\n%s", got)
	}
	if !strings.Contains(got, "name: Gemma rp v2") {
		t.Fatalf("the change was not written:\n%s", got)
	}
	// The other entry is untouched.
	if !strings.Contains(got, "id: jina-rerank-cpu") {
		t.Fatalf("an entry was lost:\n%s", got)
	}
}

// A field name that does not exist must be an error, not a setting that is
// quietly dropped.
func TestParseEntryYAMLRefusesUnknownFields(t *testing.T) {
	_, _, err := ParseEntryYAML("id: x\nprot: 12345\n")
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "prot") {
		t.Fatalf("the error does not name the field: %v", err)
	}
}
