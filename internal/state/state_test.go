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
		// HomeDir + RepoLog default to "no home" / "no commits" so existing
		// happy-path tests that don't care about repos see empty Repos
		// rather than panicking. Parity tests override these per case.
		HomeDir: func() (string, error) { return "", nil },
		RepoLog: func(ctx context.Context, dir string, n int) ([]string, error) { return nil, nil },
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
		Nodes: []NodeRow{
			{Node: nodes.Node{User: "hermes", Host: "claw1.local"}, HeartbeatPresent: true, HeartbeatAge: 10 * time.Second},
			{Node: nodes.Node{User: "hermes", Host: "claw2.local"}, HeartbeatPresent: true, HeartbeatAge: 10 * time.Second},
		},
	}

	snap.LaunchAgents = []LaunchAgent{{Label: "com.hermes.backup", PID: "0", LastExit: "0", Mode: "Daily 03:15", Description: "scripts/backup-hermes-state.sh"}}
	snap.LogSummary = []LogStat{{Name: "delegate.jsonl", Bytes: 17, Lines: 2, ModTime: fixedNow()}}
	snap.ConfigSummary = ConfigSummary{Path: "/test/.hermes/config.yaml", Bytes: 256, Backend: "local", Model: "nemotron"}
	snap.Productized = buildProductized(snap.LaunchAgents)
	snap.Repos = []RepoState{
		{Name: "hermes-setup", Path: "/home/op/hermes-setup", Present: true, Commits: []string{"abc1234 example commit"}},
	}

	md := string(snap.Markdown())
	for _, want := range []string{
		"# Current operational state — 20260516T153000Z",
		"**Generated from gateway**: claw3.local",
		"## Pinned version",
		"**v0.13.0**",
		"## Fleet inventory",
		"claw1.local",
		"| User | Host | Heartbeat |",
		"## Owera productized layer (cloud + operator plane)",
		"### Customer plane — `owera-cloud` on Fly.io",
		"### Operator plane — `owera-fleet` on `claw3.local`",
		"## LaunchAgents (gateway, `gui/501`)",
		"| Label | Mode | PID | What it runs |",
		"com.hermes.backup",
		"## Backup status",
		"2026-05-16T06:41:59Z",
		"## Config summary",
		"`local`",
		"## Recent JSONL logs",
		"delegate.jsonl",
		"## Repository state (3 repos, all on `main`)",
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

// TestParseLaunchctlList exercises the canonical `launchctl list` output
// shape: tab-separated PID/Status/Label columns with "-" for unassigned
// PIDs. The parser must keep com.hermes / com.owera / com.cloudflare
// labels and drop everything else.
func TestParseLaunchctlList(t *testing.T) {
	out := "PID\tStatus\tLabel\n" +
		"-\t0\tcom.hermes.backup\n" +
		"97742\t0\tcom.owera.snapshot-publish\n" +
		"659\t0\tcom.owera.heartbeats-bridge\n" +
		"97963\t0\tcom.cloudflare.cloudflared\n" +
		"-\t0\tcom.apple.something\n" +
		"123\t0\tcom.example.unrelated\n"
	got := ParseLaunchctlList(out)
	want := []string{
		"com.cloudflare.cloudflared",
		"com.hermes.backup",
		"com.owera.heartbeats-bridge",
		"com.owera.snapshot-publish",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d agents, got %d: %+v", len(want), len(got), got)
	}
	for i, a := range got {
		if a.Label != want[i] {
			t.Errorf("agents[%d]: got %q want %q", i, a.Label, want[i])
		}
	}
	// com.owera.snapshot-publish should keep its real PID.
	for _, a := range got {
		if a.Label == "com.owera.snapshot-publish" && a.PID != "97742" {
			t.Errorf("snapshot-publish PID: got %q want 97742", a.PID)
		}
	}
}

// TestAnnotateAgentsAddsModeAndDescription pins the doc-string lookup so
// renaming a label without updating launchAgentDoc is caught by tests
// rather than rendering as "—" in production.
func TestAnnotateAgentsAddsModeAndDescription(t *testing.T) {
	in := []LaunchAgent{
		{Label: "com.hermes.backup", PID: "0", LastExit: "0"},
		{Label: "com.owera.fleetctl-serve", PID: "98024", LastExit: "0"},
		{Label: "com.cloudflare.cloudflared", PID: "97963", LastExit: "0"},
		{Label: "com.unknown.thing", PID: "5", LastExit: "0"},
	}
	out := annotateAgents(in)
	if out[0].Mode != "Daily 03:15" {
		t.Errorf("backup mode: got %q", out[0].Mode)
	}
	if out[1].Mode != "KeepAlive" {
		t.Errorf("fleetctl-serve mode: got %q", out[1].Mode)
	}
	if out[2].Description == "" {
		t.Errorf("cloudflared should have a description, got empty")
	}
	if out[3].Mode != "" || out[3].Description != "" {
		t.Errorf("unknown label should keep empty annotation, got %+v", out[3])
	}
}

// TestBuildProductizedDerivesOperatorPlaneFromAgents pins the contract
// that operator-plane rows are only emitted for loaded agents — the
// table must not silently advertise a component as live when its
// underlying launchd job has been booted out.
func TestBuildProductizedDerivesOperatorPlaneFromAgents(t *testing.T) {
	// Empty agent list: no operator-plane rows; cloud-plane placeholders
	// still populated.
	p := buildProductized(nil)
	if len(p.OperatorPlaneOK) != 0 {
		t.Errorf("empty agents should yield no operator-plane rows, got %+v", p.OperatorPlaneOK)
	}
	if p.Cloud.App == "" || p.Cloud.Endpoint == "" {
		t.Errorf("cloud-plane placeholders must always populate, got %+v", p.Cloud)
	}

	// All four agents loaded: four rows in deterministic order.
	agents := []LaunchAgent{
		{Label: "com.owera.fleetctl-serve"},
		{Label: "com.cloudflare.cloudflared"},
		{Label: "com.owera.snapshot-publish"},
		{Label: "com.owera.heartbeats-bridge"},
	}
	p = buildProductized(agents)
	if len(p.OperatorPlaneOK) != 4 {
		t.Fatalf("expected 4 operator-plane rows, got %d: %+v", len(p.OperatorPlaneOK), p.OperatorPlaneOK)
	}
	// Order is fixed by buildProductized — fleetctl serve first, tunnel
	// second, then snapshot, then heartbeats.
	wantOrder := []string{
		"`fleetctl serve`",
		"Cloudflare Named Tunnel",
		"Snapshot publisher",
		"Heartbeats bridge",
	}
	for i, op := range p.OperatorPlaneOK {
		if op.Component != wantOrder[i] {
			t.Errorf("row %d: got %q want %q", i, op.Component, wantOrder[i])
		}
	}
}

// TestCollectReposWiresGitProbe ensures collectRepos correctly probes
// each of the three canonical productized repos and surfaces commits
// through the injected RepoLog function. Absent repos must yield
// Present=false rather than an error.
func TestCollectReposWiresGitProbe(t *testing.T) {
	home := "/home/op"
	dir := home + "/.hermes"
	stats := map[string]os.FileInfo{
		filepath.Join(home, "hermes-setup"): fakeFile{name: "hermes-setup", isDir: true, modTime: fixedNow()},
		filepath.Join(home, "owera-fleet"):  fakeFile{name: "owera-fleet", isDir: true, modTime: fixedNow()},
		// owera-cloud is intentionally absent so we exercise the
		// "directory missing -> Present=false" branch.
	}
	c := newFakeCollector(dir, nil, stats, nil, nil, nil)
	c.HomeDir = func() (string, error) { return home, nil }
	c.RepoLog = func(ctx context.Context, dir string, n int) ([]string, error) {
		return []string{"abc1234 example commit"}, nil
	}
	repos := c.collectRepos(context.Background())
	if len(repos) != 3 {
		t.Fatalf("expected 3 repo entries (one per canonical name), got %d", len(repos))
	}
	want := map[string]bool{
		"hermes-setup": true,
		"owera-cloud":  false,
		"owera-fleet":  true,
	}
	for _, r := range repos {
		if want[r.Name] != r.Present {
			t.Errorf("%s: present got %v want %v", r.Name, r.Present, want[r.Name])
		}
		if r.Present && len(r.Commits) != 1 {
			t.Errorf("%s: expected 1 commit, got %+v", r.Name, r.Commits)
		}
	}
}

// TestWrapNodesAppliesHeartbeatFreshness verifies the per-node
// heartbeat-freshness column comes from <hermesDir>/heartbeats/<host>.json
// mtimes, matching what STATE.md's Fleet inventory wants.
func TestWrapNodesAppliesHeartbeatFreshness(t *testing.T) {
	dir := "/test/.hermes"
	now := fixedNow()
	stats := map[string]os.FileInfo{
		filepath.Join(dir, "heartbeats", "claw1.local.json"): fakeFile{name: "claw1.local.json", modTime: now.Add(-15 * time.Second)},
		// claw2 has no heartbeat file — should render as "no heartbeat".
	}
	c := newFakeCollector(dir, nil, stats, nil, nil, nil)
	rows := c.wrapNodes([]nodes.Node{
		{User: "hermes", Host: "claw1.local"},
		{User: "hermes", Host: "claw2.local"},
	}, dir)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if !rows[0].HeartbeatPresent {
		t.Errorf("claw1 should have heartbeat, got %+v", rows[0])
	}
	if rows[0].HeartbeatAge < 14*time.Second || rows[0].HeartbeatAge > 16*time.Second {
		t.Errorf("claw1 age: want ~15s, got %v", rows[0].HeartbeatAge)
	}
	if rows[1].HeartbeatPresent {
		t.Errorf("claw2 should be missing, got %+v", rows[1])
	}
	if got := heartbeatColumn(rows[0]); got != "fresh (<30 s)" {
		t.Errorf("claw1 column: got %q", got)
	}
	if got := heartbeatColumn(rows[1]); got != "no heartbeat" {
		t.Errorf("claw2 column: got %q", got)
	}
}

// TestMarkdownStructuralParity drives Markdown() over a fully-populated
// Snapshot that mirrors what the live gateway produces, then asserts the
// presence of every required STATE.md section header + table-header row.
// This is the Phase-1 verification gate (T12.1): structural parity
// against hermes-setup/STATE.md, modulo timestamps/PIDs/narrative.
func TestMarkdownStructuralParity(t *testing.T) {
	agents := annotateAgents([]LaunchAgent{
		{Label: "com.hermes.backup", PID: "-", LastExit: "0"},
		{Label: "com.hermes.backup-worker", PID: "-", LastExit: "0"},
		{Label: "com.hermes.watchdog", PID: "-", LastExit: "0"},
		{Label: "com.owera.fleetctl-serve", PID: "98024", LastExit: "0"},
		{Label: "com.owera.snapshot-publish", PID: "97742", LastExit: "0"},
		{Label: "com.owera.heartbeats-bridge", PID: "659", LastExit: "0"},
		{Label: "com.cloudflare.cloudflared", PID: "97963", LastExit: "0"},
	})
	snap := &Snapshot{
		Generated:     fixedNow(),
		Hostname:      "claw3.local",
		HermesDir:     "/Users/claw3/.hermes",
		PinnedVersion: "v0.13.0",
		LastBackup:    "2026-05-17T06:15:08Z",
		Nodes: []NodeRow{
			{Node: nodes.Node{User: "hermes", Host: "claw1.local"}, HeartbeatPresent: true, HeartbeatAge: 12 * time.Second},
			{Node: nodes.Node{User: "hermes", Host: "claw2.local"}, HeartbeatPresent: true, HeartbeatAge: 14 * time.Second},
		},
		LaunchAgents:  agents,
		Productized:   buildProductized(agents),
		ConfigSummary: ConfigSummary{Path: "/Users/claw3/.hermes/config.yaml", Bytes: 7814, Backend: "local", Model: "nemotron"},
		LogSummary:    []LogStat{{Name: "delegate.jsonl", Lines: 2, Bytes: 17, ModTime: fixedNow()}},
		Repos: []RepoState{
			{Name: "hermes-setup", Path: "/Users/claw3/hermes-setup", Present: true, Commits: []string{"abc1234 commit a", "def5678 commit b"}},
			{Name: "owera-cloud", Path: "/Users/claw3/owera-cloud", Present: true, Commits: []string{"111 c1"}},
			{Name: "owera-fleet", Path: "/Users/claw3/owera-fleet", Present: true, Commits: []string{"222 f1"}},
		},
	}
	md := string(snap.Markdown())

	// Required section headers — order is documented order in STATE.md.
	wantHeaders := []string{
		"## Fleet inventory",
		"## Pinned version",
		"## Owera productized layer (cloud + operator plane)",
		"### Customer plane — `owera-cloud` on Fly.io",
		"### Operator plane — `owera-fleet` on `claw3.local`",
		"## LaunchAgents (gateway, `gui/501`)",
		"## Backup status",
		"## Config summary",
		"## Recent JSONL logs",
		"## Repository state (3 repos, all on `main`)",
	}
	for _, h := range wantHeaders {
		if !strings.Contains(md, h) {
			t.Errorf("missing section header %q", h)
		}
	}

	// Table-header rows: each "##" section that emits a table must
	// emit it with the canonical columns. We assert the markdown
	// header line so renaming a column is a visible test failure.
	wantTableHeaders := []string{
		"| User | Host | Heartbeat |",
		"| Property | Value |",
		"| Component | Where it lives |",
		"| Label | Mode | PID | What it runs |",
		"| File | Lines | Bytes | Modified |",
	}
	for _, h := range wantTableHeaders {
		if !strings.Contains(md, h) {
			t.Errorf("missing table header %q", h)
		}
	}

	// Per-row content: the operator-plane table must list all four
	// productized components; the LaunchAgents table must list all
	// seven labels; the Repository state section must show all three
	// repo names as code-quoted H3s.
	wantRows := []string{
		"`fleetctl serve`",
		"Cloudflare Named Tunnel",
		"Snapshot publisher",
		"Heartbeats bridge",
		"`com.hermes.backup`",
		"`com.hermes.backup-worker`",
		"`com.hermes.watchdog`",
		"`com.owera.fleetctl-serve`",
		"`com.owera.snapshot-publish`",
		"`com.owera.heartbeats-bridge`",
		"`com.cloudflare.cloudflared`",
		"### `hermes-setup`",
		"### `owera-cloud`",
		"### `owera-fleet`",
	}
	for _, r := range wantRows {
		if !strings.Contains(md, r) {
			t.Errorf("missing row content %q", r)
		}
	}

	// Heartbeat-freshness column must render as "fresh (<30 s)" given
	// the synthetic 12s/14s ages above.
	if !strings.Contains(md, "fresh (<30 s)") {
		t.Errorf("expected fresh heartbeat column, full output:\n%s", md)
	}
}
