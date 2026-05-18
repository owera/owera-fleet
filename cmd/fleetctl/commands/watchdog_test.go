package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/alerting"
	"github.com/owera/owera-fleet/internal/log"
)

// captureBackend is a test alerting backend that records every Alert.
// Mirrors the one in internal/alerting/alerting_test.go but kept local
// so we don't depend on test-internal types from another package.
type captureBackend struct {
	handles alerting.Severity
	sent    []alerting.Alert
	err     error
}

func (c *captureBackend) Name() string { return "capture" }
func (c *captureBackend) HandlesSeverity(s alerting.Severity) bool {
	return s == c.handles || c.handles == ""
}
func (c *captureBackend) Send(_ context.Context, a alerting.Alert) error {
	c.sent = append(c.sent, a)
	return c.err
}

// newTestLogger returns a *log.Logger writing to /dev/null so test
// output isn't polluted by JSONL spam.
func newTestLogger(t *testing.T) *log.Logger {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = devnull.Close() })
	return log.New(devnull)
}

// writeHeartbeat creates dir/host.json with mtime `age` ago.
func writeHeartbeat(t *testing.T, dir, host string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, host+".json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

// newWatchdogFixture stages a tmp heartbeat dir + state dir + capture
// backend and returns everything the cycle tests need.
type watchdogFixture struct {
	heartbeatsDir string
	stateDir      string
	router        *alerting.Router
	capture       *captureBackend
	logger        *log.Logger
}

func newWatchdogFixture(t *testing.T) *watchdogFixture {
	t.Helper()
	tmp := t.TempDir()
	hbDir := filepath.Join(tmp, "heartbeats")
	stDir := filepath.Join(tmp, "watchdog")
	if err := os.MkdirAll(hbDir, 0o755); err != nil {
		t.Fatalf("mkdir hb: %v", err)
	}
	if err := os.MkdirAll(stDir, 0o755); err != nil {
		t.Fatalf("mkdir st: %v", err)
	}
	cap := &captureBackend{handles: ""} // handle any severity
	r := alerting.NewRouter()
	r.AddBackend(cap)
	return &watchdogFixture{
		heartbeatsDir: hbDir,
		stateDir:      stDir,
		router:        r,
		capture:       cap,
		logger:        newTestLogger(t),
	}
}

func TestScanCycle_FreshHosts_NoAlert(t *testing.T) {
	f := newWatchdogFixture(t)
	writeHeartbeat(t, f.heartbeatsDir, "claw1.local", 30*time.Second)
	writeHeartbeat(t, f.heartbeatsDir, "claw2.local", 60*time.Second)

	stale, anyErr := scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)

	if stale != 0 {
		t.Fatalf("stale: got %d want 0", stale)
	}
	if anyErr {
		t.Fatal("unexpected error in fresh scan")
	}
	if len(f.capture.sent) != 0 {
		t.Fatalf("expected zero alerts on fresh fleet, got %d", len(f.capture.sent))
	}
}

func TestScanCycle_StaleHost_FiresCritical(t *testing.T) {
	f := newWatchdogFixture(t)
	writeHeartbeat(t, f.heartbeatsDir, "claw1.local", 30*time.Second) // fresh
	writeHeartbeat(t, f.heartbeatsDir, "claw2.local", 10*time.Minute) // stale

	stale, anyErr := scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)

	if stale != 1 {
		t.Fatalf("stale: got %d want 1", stale)
	}
	if anyErr {
		t.Fatal("unexpected error in stale scan")
	}
	if len(f.capture.sent) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(f.capture.sent))
	}
	a := f.capture.sent[0]
	if a.Severity != alerting.SeverityCritical {
		t.Errorf("severity: got %q want critical", a.Severity)
	}
	if a.Source != "watchdog" {
		t.Errorf("source: got %q want watchdog", a.Source)
	}
	if a.Dedup != "claw2.local" {
		t.Errorf("dedup: got %q want claw2.local", a.Dedup)
	}
	if !strings.Contains(a.Title, "claw2.local stale") {
		t.Errorf("title: got %q, want contains 'claw2.local stale'", a.Title)
	}
	if got := a.Labels["host"]; got != "claw2.local" {
		t.Errorf("label[host]: got %q want claw2.local", got)
	}
	// Marker file should now exist.
	if _, err := os.Stat(filepath.Join(f.stateDir, ".alerted-claw2.local")); err != nil {
		t.Errorf("alerted marker missing: %v", err)
	}
}

func TestScanCycle_DedupSuppression(t *testing.T) {
	f := newWatchdogFixture(t)
	writeHeartbeat(t, f.heartbeatsDir, "claw2.local", 10*time.Minute) // stale

	// First scan fires.
	_, _ = scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)
	if len(f.capture.sent) != 1 {
		t.Fatalf("first scan: got %d alerts want 1", len(f.capture.sent))
	}

	// Second scan within dedup window must suppress.
	stale, _ := scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)
	if stale != 1 {
		t.Fatalf("second scan: stale count got %d want 1 (still stale, just suppressed)", stale)
	}
	if len(f.capture.sent) != 1 {
		t.Fatalf("dedup should have suppressed; got %d alerts total want 1", len(f.capture.sent))
	}
}

func TestScanCycle_DedupExpires_RefiresAfterWindow(t *testing.T) {
	f := newWatchdogFixture(t)
	writeHeartbeat(t, f.heartbeatsDir, "claw2.local", 10*time.Minute) // stale

	// First scan fires.
	_, _ = scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)

	// Backdate the marker so dedup-window has expired.
	markerPath := filepath.Join(f.stateDir, ".alerted-claw2.local")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(markerPath, old, old); err != nil {
		t.Fatalf("chtimes marker: %v", err)
	}

	_, _ = scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)

	if len(f.capture.sent) != 2 {
		t.Fatalf("expected re-fire after dedup window, got %d alerts", len(f.capture.sent))
	}
}

func TestScanCycle_Recovery_FiresWarningAndClearsMarker(t *testing.T) {
	f := newWatchdogFixture(t)
	// First scan: stale + alert.
	hb := writeHeartbeat(t, f.heartbeatsDir, "claw2.local", 10*time.Minute)
	_, _ = scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)
	if len(f.capture.sent) != 1 {
		t.Fatalf("setup: expected 1 critical, got %d", len(f.capture.sent))
	}
	markerPath := filepath.Join(f.stateDir, ".alerted-claw2.local")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("marker should exist after first scan: %v", err)
	}

	// Worker recovers: bump heartbeat mtime to now.
	now := time.Now()
	if err := os.Chtimes(hb, now, now); err != nil {
		t.Fatalf("chtimes hb: %v", err)
	}

	_, _ = scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)

	if len(f.capture.sent) != 2 {
		t.Fatalf("expected 2 alerts after recovery (critical + recovered), got %d", len(f.capture.sent))
	}
	rec := f.capture.sent[1]
	if rec.Severity != alerting.SeverityWarning {
		t.Errorf("recovery severity: got %q want warning", rec.Severity)
	}
	if !strings.Contains(rec.Title, "recovered") {
		t.Errorf("recovery title: got %q, want contains 'recovered'", rec.Title)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker should be cleared after recovery, got err=%v", err)
	}
}

func TestScanCycle_AlertFailureKeepsMarkerUnwritten(t *testing.T) {
	f := newWatchdogFixture(t)
	f.capture.err = errors.New("transport down")
	writeHeartbeat(t, f.heartbeatsDir, "claw2.local", 10*time.Minute) // stale

	_, anyErr := scanCycle(context.Background(), f.heartbeatsDir, f.stateDir,
		5*time.Minute, 1*time.Hour, f.router, f.logger)
	if !anyErr {
		t.Fatal("expected anyErr=true on alert send failure")
	}
	// Marker must NOT be written when Send failed — otherwise dedup
	// would suppress next attempt indefinitely.
	markerPath := filepath.Join(f.stateDir, ".alerted-claw2.local")
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker should NOT exist after failed Send, got err=%v", err)
	}
}

func TestReadHeartbeatDir_FiltersHiddenAndNonJSON(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"claw1.local.json", "claw2.local.json", ".alerted-claw1.local", "scratch.txt", ".DS_Store"} {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	ents, err := readHeartbeatDir(tmp)
	if err != nil {
		t.Fatalf("readHeartbeatDir: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("got %d entries want 2", len(ents))
	}
	hosts := []string{ents[0].Host, ents[1].Host}
	if hosts[0] != "claw1.local" || hosts[1] != "claw2.local" {
		t.Errorf("hosts: got %v want [claw1.local claw2.local]", hosts)
	}
}

func TestBuildAlertingRouterFromEnv_NoBackendsWhenEnvUnset(t *testing.T) {
	t.Setenv("HERMES_PAGERDUTY_ROUTING_KEY", "")
	t.Setenv("HERMES_OPSGENIE_API_KEY", "")
	t.Setenv("HERMES_NTFY_URL", "")
	r, names := buildAlertingRouterFromEnv()
	if r == nil {
		t.Fatal("router nil")
	}
	if len(names) != 0 {
		t.Errorf("backends: got %v want none", names)
	}
}

func TestBuildAlertingRouterFromEnv_WiresEachBackendOnDemand(t *testing.T) {
	t.Setenv("HERMES_PAGERDUTY_ROUTING_KEY", "rk_test")
	t.Setenv("HERMES_OPSGENIE_API_KEY", "og_test")
	t.Setenv("HERMES_NTFY_URL", "https://ntfy.example/topic")
	_, names := buildAlertingRouterFromEnv()
	if len(names) != 3 {
		t.Fatalf("backends: got %d want 3 (%v)", len(names), names)
	}
}

// ensure io is referenced even if some test path drops its use; keeps
// the import set stable across edits.
var _ = io.Discard
