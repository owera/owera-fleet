package commands

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// writeFixedFile writes a file at path with `size` zero-bytes payload.
// Returns the path for convenience.
func writeFixedFile(t *testing.T, path string, size int) string {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// readGzip returns the decompressed contents of a .gz file.
func readGzip(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()
	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gz: %v", err)
	}
	return body
}

type logrotateFixture struct {
	dir        string
	selfLogPath string
	logger     *flogger
}

// flogger is a stand-in *log.Logger that writes to /dev/null; we use
// the real `log.New(devnull)` constructor via newTestLogger from
// watchdog_test.go to keep the test helpers consistent across files.
type flogger struct{}

func newLogrotateFixture(t *testing.T) *logrotateFixture {
	t.Helper()
	tmp := t.TempDir()
	// Self-log path that the rotator will skip.
	self := filepath.Join(tmp, "owera-logrotate.jsonl")
	if err := os.WriteFile(self, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write self log: %v", err)
	}
	return &logrotateFixture{
		dir:        tmp,
		selfLogPath: self,
	}
}

func TestRotateAll_SkipsBelowThreshold(t *testing.T) {
	f := newLogrotateFixture(t)
	small := writeFixedFile(t, filepath.Join(f.dir, "small.jsonl"), 100)
	logger := newTestLogger(t)

	summary, err := rotateAll(context.Background(), f.dir, f.selfLogPath, 1024, 5, false, logger)
	if err != nil {
		t.Fatalf("rotateAll: %v", err)
	}
	if summary.Rotated != 0 {
		t.Errorf("rotated: got %d want 0", summary.Rotated)
	}
	if summary.Skipped != 1 {
		t.Errorf("skipped: got %d want 1", summary.Skipped)
	}
	if summary.Errors != 0 {
		t.Errorf("errors: got %d want 0", summary.Errors)
	}
	// small.jsonl must be untouched.
	info, err := os.Stat(small)
	if err != nil || info.Size() != 100 {
		t.Errorf("small file modified: size=%d err=%v", info.Size(), err)
	}
}

func TestRotateAll_RotatesAboveThreshold(t *testing.T) {
	f := newLogrotateFixture(t)
	src := writeFixedFile(t, filepath.Join(f.dir, "big.jsonl"), 2048)
	logger := newTestLogger(t)

	summary, err := rotateAll(context.Background(), f.dir, f.selfLogPath, 1024, 5, false, logger)
	if err != nil {
		t.Fatalf("rotateAll: %v", err)
	}
	if summary.Rotated != 1 {
		t.Fatalf("rotated: got %d want 1", summary.Rotated)
	}
	// Source must be truncated.
	info, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("src not truncated: size=%d", info.Size())
	}
	// Exactly one .gz produced.
	matches, _ := filepath.Glob(src + ".*.gz")
	if len(matches) != 1 {
		t.Fatalf("expected 1 .gz, got %d (%v)", len(matches), matches)
	}
	// gz contents must match the original.
	got := readGzip(t, matches[0])
	if !bytes.Equal(got, bytes.Repeat([]byte{'x'}, 2048)) {
		t.Errorf("gz contents differ from original")
	}
}

func TestRotateAll_SkipsSelfLogAndNonJSONL(t *testing.T) {
	f := newLogrotateFixture(t)
	// self log is f.selfLogPath, already > 0 bytes
	writeFixedFile(t, f.selfLogPath, 5000) // grow self above threshold
	writeFixedFile(t, filepath.Join(f.dir, "agent.log"), 5000)
	writeFixedFile(t, filepath.Join(f.dir, "rotate.jsonl"), 5000) // legacy bash rotator self-log
	writeFixedFile(t, filepath.Join(f.dir, "other.txt"), 5000)
	logger := newTestLogger(t)

	summary, err := rotateAll(context.Background(), f.dir, f.selfLogPath, 1024, 5, false, logger)
	if err != nil {
		t.Fatalf("rotateAll: %v", err)
	}
	// Only rotate.jsonl is *.jsonl AND not the self-log AND above threshold.
	if summary.Rotated != 1 {
		t.Errorf("rotated: got %d want 1 (only rotate.jsonl should rotate)", summary.Rotated)
	}
	// Confirm self-log unchanged (no .gz next to it, original size intact).
	info, err := os.Stat(f.selfLogPath)
	if err != nil || info.Size() != 5000 {
		t.Errorf("self log modified: size=%d err=%v", info.Size(), err)
	}
	matches, _ := filepath.Glob(f.selfLogPath + ".*.gz")
	if len(matches) != 0 {
		t.Errorf("self log was rotated: %v", matches)
	}
	// agent.log is not *.jsonl so untouched.
	info, err = os.Stat(filepath.Join(f.dir, "agent.log"))
	if err != nil || info.Size() != 5000 {
		t.Errorf("agent.log modified: size=%d err=%v", info.Size(), err)
	}
}

func TestRotateAll_DryRunMakesNoFSChanges(t *testing.T) {
	f := newLogrotateFixture(t)
	src := writeFixedFile(t, filepath.Join(f.dir, "big.jsonl"), 4096)
	logger := newTestLogger(t)

	summary, err := rotateAll(context.Background(), f.dir, f.selfLogPath, 1024, 5, true, logger)
	if err != nil {
		t.Fatalf("rotateAll dry-run: %v", err)
	}
	if summary.Rotated != 1 {
		t.Errorf("rotated count should still reflect intent in dry-run: got %d want 1", summary.Rotated)
	}
	// Source untouched.
	info, err := os.Stat(src)
	if err != nil || info.Size() != 4096 {
		t.Errorf("dry-run modified src: size=%d err=%v", info.Size(), err)
	}
	// No .gz produced.
	matches, _ := filepath.Glob(src + ".*.gz")
	if len(matches) != 0 {
		t.Errorf("dry-run produced %d gz files: %v", len(matches), matches)
	}
}

func TestPruneRotations_KeepsNewestK(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "watchdog.jsonl")
	// Create 7 fake rotations with sortable timestamps in the filename.
	stamps := []string{
		"20260518T100000Z", "20260518T110000Z", "20260518T120000Z",
		"20260518T130000Z", "20260518T140000Z", "20260518T150000Z",
		"20260518T160000Z",
	}
	for _, s := range stamps {
		p := fmt.Sprintf("%s.%s.gz", base, s)
		if err := os.WriteFile(p, []byte("g"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	pruned, err := pruneRotations(base, 3)
	if err != nil {
		t.Fatalf("pruneRotations: %v", err)
	}
	if pruned != 4 {
		t.Errorf("pruned: got %d want 4", pruned)
	}
	// Only the 3 newest survive.
	matches, _ := filepath.Glob(base + ".*.gz")
	sort.Strings(matches)
	if len(matches) != 3 {
		t.Fatalf("survivors: got %d want 3 (%v)", len(matches), matches)
	}
	wantTails := []string{"20260518T140000Z.gz", "20260518T150000Z.gz", "20260518T160000Z.gz"}
	for i, m := range matches {
		if !strings.HasSuffix(m, wantTails[i]) {
			t.Errorf("survivor[%d]: got %q want suffix %q", i, m, wantTails[i])
		}
	}
}

func TestPruneRotations_NoopWhenBelowKeep(t *testing.T) {
	tmp := t.TempDir()
	base := filepath.Join(tmp, "x.jsonl")
	for _, s := range []string{"20260518T100000Z", "20260518T110000Z"} {
		p := fmt.Sprintf("%s.%s.gz", base, s)
		if err := os.WriteFile(p, []byte("g"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	pruned, err := pruneRotations(base, 5)
	if err != nil {
		t.Fatalf("pruneRotations: %v", err)
	}
	if pruned != 0 {
		t.Errorf("pruned: got %d want 0", pruned)
	}
	matches, _ := filepath.Glob(base + ".*.gz")
	if len(matches) != 2 {
		t.Errorf("matches: got %d want 2", len(matches))
	}
}

func TestRotateOne_AtomicAndPreservesContents(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "a.jsonl")
	body := []byte(`{"line":1}` + "\n" + `{"line":2}` + "\n")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	at := time.Date(2026, 5, 18, 13, 45, 30, 0, time.UTC)
	got, err := rotateOne(src, at)
	if err != nil {
		t.Fatalf("rotateOne: %v", err)
	}
	wantSuffix := ".20260518T134530Z.gz"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("rotation name: got %q want suffix %q", got, wantSuffix)
	}
	// gz contents == original.
	gotBody := readGzip(t, got)
	if !bytes.Equal(gotBody, body) {
		t.Errorf("gz contents differ from original")
	}
	// Source truncated.
	info, err := os.Stat(src)
	if err != nil || info.Size() != 0 {
		t.Errorf("src not truncated: size=%d err=%v", info.Size(), err)
	}
	// No .rotating.* leftovers.
	leftovers, _ := filepath.Glob(filepath.Join(tmp, "*.rotating.*"))
	if len(leftovers) != 0 {
		t.Errorf("temp leftovers: %v", leftovers)
	}
}

func TestResolveThresholdBytes_FlagBeatsEnv(t *testing.T) {
	t.Setenv("HERMES_LOG_ROTATE_THRESHOLD", "999")
	defer func() { logrotateThresholdBytes = 0 }()
	logrotateThresholdBytes = 0
	if got := resolveThresholdBytes(); got != 999 {
		t.Errorf("env only: got %d want 999", got)
	}
	logrotateThresholdBytes = 4242
	if got := resolveThresholdBytes(); got != 4242 {
		t.Errorf("flag overrides env: got %d want 4242", got)
	}
	logrotateThresholdBytes = 0
	t.Setenv("HERMES_LOG_ROTATE_THRESHOLD", "")
	if got := resolveThresholdBytes(); got != 1<<20 {
		t.Errorf("default: got %d want 1 MiB", got)
	}
}

func TestResolveKeep_FlagBeatsEnv(t *testing.T) {
	t.Setenv("HERMES_LOG_ROTATE_KEEP", "12")
	defer func() { logrotateKeep = 0 }()
	logrotateKeep = 0
	if got := resolveKeep(); got != 12 {
		t.Errorf("env only: got %d want 12", got)
	}
	logrotateKeep = 7
	if got := resolveKeep(); got != 7 {
		t.Errorf("flag overrides env: got %d want 7", got)
	}
	logrotateKeep = 0
	t.Setenv("HERMES_LOG_ROTATE_KEEP", "")
	if got := resolveKeep(); got != 5 {
		t.Errorf("default: got %d want 5", got)
	}
}

// ensure the dead-import warning never trips; the *flogger* type exists
// only to keep the file's intent obvious to future readers and is not
// instantiated by any test.
var _ = (*flogger)(nil)
