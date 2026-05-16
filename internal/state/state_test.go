package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/nodes"
)

// fakeFile is an in-memory os.FileInfo so collectLogs / collectConfig run
// against synthetic data without touching the operator's real ~/.hermes/.
type fakeFile struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (f fakeFile) Name() string       { return f.name }
func (f fakeFile) Size() int64        { return f.size }
func (f fakeFile) Mode() os.FileMode  { return 0o644 }
func (f fakeFile) ModTime() time.Time { return f.modTime }
func (f fakeFile) IsDir() bool        { return f.isDir }
func (f fakeFile) Sys() any           { return nil }

func fixedNow() time.Time {
	t, _ := time.Parse(time.RFC3339, "2026-05-16T15:30:00Z")
	return t
}

func newFakeCollector(hermesDir string, files map[string][]byte, stats map[string]os.FileInfo, logs []os.FileInfo, agents []LaunchAgent, agentsErr error) *Collector {
	return &Collector{
		HermesDir: hermesDir,
		Now:       fixedNow,
		Hostname:  func() (string, error) { return "claw3.local", nil },
		ReadFile: func(path string) ([]byte, error) {
			data, ok := files[path]
			if !ok {
				return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
			}
			return data, nil
		},
		StatFile: func(path string) (os.FileInfo, error) {
			info, ok := stats[path]
			if !ok {
				return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
			}
			return info, nil
		},
		ListLogs: func(dir string) ([]os.FileInfo, error) {
			return logs, nil
		},
		LaunchAgent: func(ctx context.Context) ([]LaunchAgent, error) {
			return agents, agentsErr
		},
	}
}

func TestCollectHappyPath(t *testing.T) {
	dir := "/test/.hermes"
	now := fixedNow()
	files := map[string][]byte{
		filepath.Join(dir, "PINNED_VERSION"):      []byte("v0.13.0\n"),
		filepath.Join(dir, "PINNED_VERSION.prev"): []byte("v0.12.4\n"),
		filepath.Join(dir, "LAST_BACKUP"):         []byte("2026-05-16T06:41:59Z\n"),
		filepath.Join(dir, "nodes.txt"):           []byte("hermes@claw1.local\nhermes@claw2.local\n"),
		filepath.Join(dir, "config.yaml"):         []byte("model:\n  default: nvidia/nemotron-3-super-120b-a12b:free\nsandbox:\n  backend: local\n"),
	}
	configInfo := fakeFile{name: "config.yaml", size: int64(len(files[filepath.Join(dir, "config.yaml")])), modTime: now}
	stats := map[string]os.FileInfo{
		filepath.Join(dir, "config.yaml"): configInfo,
	}
	logFile := fakeFile{name: "delegate.jsonl", size: 17, modTime: now}
	files[filepath.Join(dir, "logs", "delegate.jsonl")] = []byte("{\"a\":1}\n{\"b\":2}\n")
	logs := []os.FileInfo{logFile}
	agents := []LaunchAgent{{Label: "com.hermes.backup", PID: "0", LastExit: "0"}}

	c := newFakeCollector(dir, files, stats, logs, agents, nil)
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap.PinnedVersion != "v0.13.0" {
		t.Errorf("pinned: got %q", snap.PinnedVersion)
	}
	if snap.PrevVersion != "v0.12.4" {
		t.Errorf("prev: got %q", snap.PrevVersion)
	}
	if snap.LastBackup != "2026-05-16T06:41:59Z" {
		t.Errorf("last_backup: got %q", snap.LastBackup)
	}
	if len(snap.Nodes) != 2 || snap.Nodes[0].Host != "claw1.local" || snap.Nodes[1].Host != "claw2.local" {
		t.Errorf("nodes: got %+v", snap.Nodes)
	}
	if snap.ConfigSummary.Backend != "local" {
		t.Errorf("backend: got %q", snap.ConfigSummary.Backend)
	}
	if snap.ConfigSummary.Model != "nvidia/nemotron-3-super-120b-a12b:free" {
		t.Errorf("model: got %q", snap.ConfigSummary.Model)
	}
	if len(snap.LaunchAgents) != 1 {
		t.Fatalf("agents: got %+v", snap.LaunchAgents)
	}
	if len(snap.LogSummary) != 1 || snap.LogSummary[0].Lines != 2 {
		t.Errorf("logs: got %+v", snap.LogSummary)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("expected no probe errors, got %+v", snap.Errors)
	}
}

func TestCollectMissingPinnedIsNotAnError(t *testing.T) {
	dir := "/test/.hermes"
	c := newFakeCollector(dir, nil, nil, nil, nil, nil)
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap.PinnedVersion != "" || snap.PrevVersion != "" || snap.LastBackup != "" {
		t.Errorf("missing files should produce empty fields, got %+v", snap)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("ENOENT must not register as a probe error, got %+v", snap.Errors)
	}
}

func TestCollectLaunchctlErrorRecorded(t *testing.T) {
	dir := "/test/.hermes"
	c := newFakeCollector(dir, nil, nil, nil, nil, errors.New("launchctl not found"))
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Errors) != 1 || snap.Errors[0].Probe != "launch_agents" {
		t.Errorf("expected launch_agents probe error, got %+v", snap.Errors)
	}
}

func TestParseLaunchctlPrint(t *testing.T) {
	out := `services = {
	     1234   0   com.hermes.backup
	        0   0   com.hermes.watchdog
	     -      0   com.apple.something
	     5678   0   com.hermes.backup-worker
}
some.other = thing
`
	agents := ParseLaunchctlPrint(out)
	if len(agents) != 3 {
		t.Fatalf("expected 3 com.hermes.* services, got %d: %+v", len(agents), agents)
	}
	// Sorted alphabetically.
	want := []string{"com.hermes.backup", "com.hermes.backup-worker", "com.hermes.watchdog"}
	for i, a := range agents {
		if a.Label != want[i] {
			t.Errorf("agents[%d]: got %q want %q", i, a.Label, want[i])
		}
	}
}

func TestMarkdownRenders(t *testing.T) {
	snap := &Snapshot{
		Generated:     fixedNow(),
		Hostname:      "claw3.local",
		HermesDir:     "/test/.hermes",
		PinnedVersion: "v0.13.0",
		LastBackup:    "2026-05-16T06:41:59Z",
		Nodes: []nodes.Node{
			{User: "hermes", Host: "claw1.local"},
			{User: "hermes", Host: "claw2.local"},
		},
	}

	snap.LaunchAgents = []LaunchAgent{{Label: "com.hermes.backup", PID: "0", LastExit: "0"}}
	snap.LogSummary = []LogStat{{Name: "delegate.jsonl", Bytes: 17, Lines: 2, ModTime: fixedNow()}}
	snap.ConfigSummary = ConfigSummary{Path: "/test/.hermes/config.yaml", Bytes: 256, Backend: "local", Model: "nemotron"}

	md := string(snap.Markdown())
	for _, want := range []string{
		"# Current operational state — 20260516T153000Z",
		"**Generated from gateway**: claw3.local",
		"## Pinned version",
		"**v0.13.0**",
		"## Fleet inventory",
		"claw1.local",
		"## LaunchAgents (gateway, user domain)",
		"com.hermes.backup",
		"## Backup status",
		"2026-05-16T06:41:59Z",
		"## Config summary",
		"`local`",
		"## Recent JSONL logs",
		"delegate.jsonl",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\nfull output:\n%s", want, md)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	snap := &Snapshot{
		Generated:     fixedNow(),
		Hostname:      "claw3.local",
		HermesDir:     "/test/.hermes",
		PinnedVersion: "v0.13.0",
	}
	data, err := snap.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Snapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PinnedVersion != snap.PinnedVersion {
		t.Errorf("roundtrip lost data: %+v", got)
	}
}

func TestCollectNoHomeNoDir(t *testing.T) {
	t.Setenv("HOME", "")
	c := &Collector{Now: fixedNow}
	_, err := c.Collect(context.Background())
	if !errors.Is(err, ErrNoHome) {
		t.Errorf("expected ErrNoHome, got %v", err)
	}
}
