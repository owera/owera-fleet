package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owera/owera-fleet/internal/markers"
)

// resetMarkersFlags clears the package-level flag globals so a test that
// runs alongside others (or after) doesn't inherit values from a previous
// invocation. Cobra flag state is process-wide; reset it explicitly.
func resetMarkersFlags() {
	markersDir = ""
	markersPipeline = ""
	markersStep = ""
	markersInput = ""
	markersOutput = ""
	markersFormat = ""
}

func TestMarkersHashWritesMarkerAndPrintsHash(t *testing.T) {
	resetMarkersFlags()
	defer resetMarkersFlags()

	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("hello-markers\n"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	markersDir = dir
	markersPipeline = "etl"
	markersStep = "load_users"
	markersInput = input

	var buf bytes.Buffer
	markersHashCmd.SetOut(&buf)
	defer markersHashCmd.SetOut(nil)

	if err := runMarkersHash(markersHashCmd, nil); err != nil {
		t.Fatalf("runMarkersHash: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "sha256:") {
		t.Errorf("expected sha256: hash on stdout, got %q", buf.String())
	}

	// Marker should exist on disk and be readable.
	store, err := markers.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	m, err := store.Read("etl", "load_users")
	if err != nil {
		t.Fatalf("Read marker: %v", err)
	}
	if !strings.HasPrefix(m.InputHash, "sha256:") {
		t.Errorf("marker hash = %q", m.InputHash)
	}
}

func TestMarkersVerifyFreshAndStale(t *testing.T) {
	resetMarkersFlags()
	defer resetMarkersFlags()

	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	markersDir = dir
	markersPipeline = "p"
	markersStep = "s"
	markersInput = input

	// Hash to create the marker.
	var hashBuf bytes.Buffer
	markersHashCmd.SetOut(&hashBuf)
	if err := runMarkersHash(markersHashCmd, nil); err != nil {
		t.Fatalf("hash: %v", err)
	}
	markersHashCmd.SetOut(nil)

	// Verify with same input → fresh.
	var verifyBuf bytes.Buffer
	markersVerifyCmd.SetOut(&verifyBuf)
	if err := runMarkersVerify(markersVerifyCmd, nil); err != nil {
		t.Fatalf("verify fresh: unexpected error: %v", err)
	}
	if !strings.Contains(verifyBuf.String(), "fresh") {
		t.Errorf("expected 'fresh' on stdout, got %q", verifyBuf.String())
	}
	markersVerifyCmd.SetOut(nil)

	// Mutate the input, then verify → must return an error (exit 1).
	if err := os.WriteFile(input, []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite input: %v", err)
	}
	err := runMarkersVerify(markersVerifyCmd, nil)
	if err == nil {
		t.Errorf("expected stale error after input changed, got nil")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Errorf("expected 'stale' in error, got %v", err)
	}
}

func TestMarkersListJSON(t *testing.T) {
	resetMarkersFlags()
	defer resetMarkersFlags()

	dir := t.TempDir()
	store, err := markers.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, step := range []string{"a", "b"} {
		if err := store.Write(markers.Marker{Pipeline: "etl", Step: step, InputHash: "sha256:x"}); err != nil {
			t.Fatalf("write marker %s: %v", step, err)
		}
	}

	markersDir = dir
	markersPipeline = "etl"
	markersFormat = "json"

	var buf bytes.Buffer
	markersListCmd.SetOut(&buf)
	defer markersListCmd.SetOut(nil)

	if err := runMarkersList(markersListCmd, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []markers.Marker
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal JSON: %v\nraw: %s", err, buf.Bytes())
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 markers, got %d", len(got))
	}
	if got[0].Step != "a" || got[1].Step != "b" {
		t.Errorf("expected sorted steps a,b, got %q,%q", got[0].Step, got[1].Step)
	}
}

func TestMarkersListEmptyJSONIsArrayNotNull(t *testing.T) {
	resetMarkersFlags()
	defer resetMarkersFlags()

	markersDir = t.TempDir()
	markersPipeline = "nope"
	markersFormat = "json"

	var buf bytes.Buffer
	markersListCmd.SetOut(&buf)
	defer markersListCmd.SetOut(nil)

	if err := runMarkersList(markersListCmd, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("expected '[]' for empty list, got %q", got)
	}
}

func TestMarkersInvalidateIdempotent(t *testing.T) {
	resetMarkersFlags()
	defer resetMarkersFlags()

	dir := t.TempDir()
	store, _ := markers.NewStore(dir)
	_ = store.Write(markers.Marker{Pipeline: "p", Step: "s", InputHash: "h"})

	markersDir = dir
	markersPipeline = "p"
	markersStep = "s"

	var buf bytes.Buffer
	markersInvalidateCmd.SetOut(&buf)
	defer markersInvalidateCmd.SetOut(nil)

	if err := runMarkersInvalidate(markersInvalidateCmd, nil); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := store.Read("p", "s"); err == nil {
		t.Error("marker should be gone after invalidate")
	}
	// Idempotent — second call must also succeed.
	if err := runMarkersInvalidate(markersInvalidateCmd, nil); err != nil {
		t.Errorf("invalidate should be idempotent: %v", err)
	}
}

func TestMarkersHashStdin(t *testing.T) {
	resetMarkersFlags()
	defer resetMarkersFlags()

	dir := t.TempDir()
	markersDir = dir
	markersPipeline = "p"
	markersStep = "s"
	markersInput = "-"

	var out bytes.Buffer
	markersHashCmd.SetIn(strings.NewReader("from-stdin"))
	markersHashCmd.SetOut(&out)
	defer func() {
		markersHashCmd.SetIn(nil)
		markersHashCmd.SetOut(nil)
	}()

	if err := runMarkersHash(markersHashCmd, nil); err != nil {
		t.Fatalf("hash from stdin: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "sha256:") {
		t.Errorf("expected sha256 prefix, got %q", out.String())
	}
}
