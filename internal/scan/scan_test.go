package scan

import (
	"os"
	"path/filepath"
	"testing"
)

// writeScript drops a script into a temp dir and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

// A launcher that hides its port behind the shell's default expansion used to
// come back with port 0, which made the entry impossible to import.
func TestParseShellDefaultExpansion(t *testing.T) {
	p := writeScript(t, `#!/usr/bin/env zsh
set -e
PROJECT="/home/roychong/app/speech.cpp"
BIN="$PROJECT/build_CUDA/speech"
MODEL="${MODEL:-qwen3-tts-0.6b:q8_0}"
BIND_HOST="${BIND_HOST:-0.0.0.0}"
PORT="${PORT:-11439}"
DEVICE="${DEVICE:-CUDA0}"

exec "$BIN" serve "$MODEL" \
  --host "$BIND_HOST" \
  --port "$PORT" \
  --device "$DEVICE"
`)
	c, err := ParseScript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 11439 {
		t.Errorf("port = %d, want 11439", c.Port)
	}
	if c.Host != "0.0.0.0" {
		t.Errorf("host = %q, want 0.0.0.0", c.Host)
	}
	if c.Binary != "/home/roychong/app/speech.cpp/build_CUDA/speech" {
		t.Errorf("binary = %q, want the resolved path", c.Binary)
	}
	if c.Ready != "http" {
		t.Errorf("ready = %q, want http", c.Ready)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", c.Warnings)
	}
}

// ${VAR-default} (unset only) and ${VAR:=default} (assign) behave the same
// way for a static reader.
func TestParseShellDefaultVariants(t *testing.T) {
	p := writeScript(t, `#!/bin/sh
PORT="${PORT-11440}"
HOST="${HOST:=127.0.0.1}"
exec /usr/bin/speech serve --port "$PORT" --host "$HOST"
`)
	c, err := ParseScript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 11440 {
		t.Errorf("port = %d, want 11440", c.Port)
	}
	if c.Host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", c.Host)
	}
}

// Quoted flag values are just as common as bare ones.
func TestParseQuotedFlags(t *testing.T) {
	p := writeScript(t, `#!/bin/sh
exec /usr/bin/llama-server --model "/models/x.gguf" --port "8080" --host "0.0.0.0"
`)
	c, err := ParseScript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 8080 {
		t.Errorf("port = %d, want 8080", c.Port)
	}
	if c.Host != "0.0.0.0" {
		t.Errorf("host = %q, want 0.0.0.0", c.Host)
	}
	if c.Model != "/models/x.gguf" {
		t.Errorf("model = %q, want /models/x.gguf", c.Model)
	}
}

// The existing fixture must keep parsing the way it always did.
func TestParseFixture(t *testing.T) {
	c, err := ParseScript("../../testdata/scan/sample-chat.sh")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 22999 {
		t.Errorf("port = %d, want 22999", c.Port)
	}
	if c.Host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", c.Host)
	}
	if c.Placement != "gpu" {
		t.Errorf("placement = %q, want gpu", c.Placement)
	}
	want := map[string]bool{"-ngl 99": false, "-c 4096": false, "--jinja": false}
	for _, f := range c.Flags {
		if _, ok := want[f]; ok {
			want[f] = true
		}
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("flag %q missing from %v", f, c.Flags)
		}
	}
}

// A JSON config file carries the port, not a flag.
func TestParseJSONPort(t *testing.T) {
	p := writeScript(t, `#!/bin/sh
CONFIG=/home/roychong/config.json
exec /usr/bin/audiocpp_server --config "$CONFIG"
`)
	c, err := ParseScript(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The fixture config does not exist, so the port stays unknown — but the
	// candidate must still be produced with a warning, not an error.
	if c.Ready != "none" {
		t.Errorf("ready = %q, want none", c.Ready)
	}
	if len(c.Warnings) == 0 {
		t.Error("expected a warning about the missing port")
	}
}
