package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteJSONAtomic_FreshFile verifies that writeJSONAtomic produces
// a valid file with the marshalled payload when no previous file
// exists.
func TestWriteJSONAtomic_FreshFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	payload := map[string]any{
		"ts":     "2026-05-17T12:00:00Z",
		"status": "operational",
	}
	if err := writeJSONAtomic(out, payload); err != nil {
		t.Fatalf("writeJSONAtomic: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if got["status"] != "operational" {
		t.Errorf("status: %v want operational", got["status"])
	}
}

// TestWriteJSONAtomic_OverwritesPrevious verifies that calling
// writeJSONAtomic a second time replaces the file contents wholesale.
// The atomic rename means a concurrent reader sees either the old
// complete file or the new complete file — never partial bytes.
func TestWriteJSONAtomic_OverwritesPrevious(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	if err := writeJSONAtomic(out, map[string]any{"v": 1}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeJSONAtomic(out, map[string]any{"v": 2, "extra": "y"}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	raw, _ := os.ReadFile(out)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if got["v"].(float64) != 2 {
		t.Errorf("v: %v want 2", got["v"])
	}
	if got["extra"] != "y" {
		t.Errorf("extra: %v want y", got["extra"])
	}

	// The tmp sibling should NOT exist post-rename.
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind: %v", err)
	}
}

// TestWriteJSONAtomic_PrettyPrinted verifies the output is human-
// readable JSON (indented), which matters for operator debugging via
// `cat`. We don't lock the exact whitespace shape — just confirm
// MarshalIndent's newline/indent is present.
func TestWriteJSONAtomic_PrettyPrinted(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	if err := writeJSONAtomic(out, map[string]any{"a": 1, "b": 2}); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, _ := os.ReadFile(out)
	s := string(raw)
	if !strings.Contains(s, "\n") {
		t.Errorf("expected indented JSON with newlines; got %q", s)
	}
}

// TestWriteJSONAtomic_RejectsUnmarshallable ensures encoding errors
// surface from writeJSONAtomic without leaving a .tmp behind.
func TestWriteJSONAtomic_RejectsUnmarshallable(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "snap.json")

	// channels can't be JSON-marshalled.
	err := writeJSONAtomic(out, make(chan int))
	if err == nil {
		t.Fatal("expected error on unmarshallable input")
	}
	// No .tmp residue.
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind on error: %v", err)
	}
	// No output file either.
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("out file created despite marshal error: %v", err)
	}
}
