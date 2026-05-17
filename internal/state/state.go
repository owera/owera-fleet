// Package state collects a point-in-time snapshot of the live Hermes
// gateway and renders it as markdown or JSON. It is the typed replacement
// for the hand-edited STATE.md drift that hermes-setup carried.
//
// The collector is purposely structured around small, independently
// overridable probes so tests can substitute synthetic data without poking
// at the operator's real ~/.hermes/ tree or invoking launchctl. Production
// callers use Default() to wire the probes against the actual filesystem
// and macOS tooling; callers in tests construct a Collector directly with
// fakes.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/owera/owera-fleet/internal/nodes"
	"github.com/owera/owera-fleet/internal/report"
)

// Snapshot is the structured result of one collection pass. Every field is
// optional; missing data is represented by zero values + an entry in Errors
// so renderers can show "—" instead of failing the whole run.
type Snapshot struct {
	Generated     time.Time     `json:"generated"`
	Hostname      string        `json:"hostname"`
	HermesDir     string        `json:"hermes_dir"`
	PinnedVersion string        `json:"pinned_version,omitempty"`
	PrevVersion   string        `json:"prev_version,omitempty"`
	LastBackup    string        `json:"last_backup,omitempty"`
	Nodes         []NodeRow     `json:"nodes"`
	LaunchAgents  []LaunchAgent `json:"launch_agents"`
	Productized   Productized   `json:"productized"`
	Repos         []RepoState   `json:"repos"`
	LogSummary    []LogStat     `json:"log_summary"`
	ConfigSummary ConfigSummary `json:"config_summary"`
	Errors        []ProbeError  `json:"errors,omitempty"`
}

// NodeRow is one row in the Fleet inventory table. It wraps the parsed
// nodes.Node with the heartbeat-freshness probe result so the renderer
// can emit STATE.md's per-worker status column.
type NodeRow struct {
	nodes.Node
	// HeartbeatAge is the duration since ~/.hermes/heartbeats/<host>.json
	// was last touched. Zero means "no heartbeat file present".
	HeartbeatAge time.Duration `json:"heartbeat_age_ns,omitempty"`
	// HeartbeatPresent reports whether the heartbeat file exists at all.
	// Distinguishes "stale but present" from "never seen".
	HeartbeatPresent bool `json:"heartbeat_present"`
}

// LaunchAgent is one entry in the gateway's user-domain launchctl list.
//
// PID/LastExit come from `launchctl list` (which yields PID, LastExit,
// Label). Mode + Description are stable per-label annotations derived
// from launchAgentDoc so the renderer can produce STATE.md's
// "Label | Mode | What it runs | Last status" shape without re-probing.
type LaunchAgent struct {
	Label       string `json:"label"`
	PID         string `json:"pid,omitempty"`
	LastExit    string `json:"last_exit,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Description string `json:"description,omitempty"`
}

// Productized captures the "Owera productized layer (cloud + operator
// plane)" section from STATE.md. The cloud-plane facts are intentionally
// hardcoded placeholders today (no probe path exists to fetch Fly app
// metadata from the gateway without authenticated flyctl); the
// operator-plane component list is derived from which com.owera.*
// LaunchAgents are loaded so the table stays honest if an agent gets
// uninstalled.
type Productized struct {
	Cloud           CloudPlane      `json:"cloud"`
	OperatorPlaneOK []OperatorPlane `json:"operator_plane"`
}

// CloudPlane mirrors the "Customer plane — owera-cloud on Fly.io" table
// in STATE.md. Values are placeholders today; future revisions will pick
// them up from env (FLY_APP_NAME, OPERATOR_RPC_URL) or a config file.
type CloudPlane struct {
	App           string `json:"app"`
	Endpoint      string `json:"endpoint"`
	HealthCheck   string `json:"health_check"`
	Auth          string `json:"auth"`
	Billing       string `json:"billing"`
	OperatorRPC   string `json:"operator_rpc"`
	AdminEndpoint string `json:"admin_endpoint"`
	BootFinger    string `json:"boot_fingerprint"`
}

// OperatorPlane is one row in the "Operator plane — owera-fleet on
// claw3.local" table from STATE.md.
type OperatorPlane struct {
	Component string `json:"component"`
	Where     string `json:"where"`
}

// RepoState captures the git head of one of the three productized repos
// for the "Repository state" section. Path is the absolute checkout dir,
// Name is the human-readable label, Commits is the most-recent N commits
// (each line: "<sha> <subject>"). Present is false when the directory
// doesn't exist on this gateway, which makes the renderer skip gracefully
// rather than emit an empty table for the absent repo.
type RepoState struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Present bool     `json:"present"`
	Commits []string `json:"commits,omitempty"`
}

// LogStat summarizes one JSONL file under ~/.hermes/logs/.
type LogStat struct {
	Name    string    `json:"name"`
	Bytes   int64     `json:"bytes"`
	Lines   int64     `json:"lines"`
	ModTime time.Time `json:"mod_time"`
}

// ConfigSummary captures load-bearing facts about ~/.hermes/config.yaml
// without parsing the full schema. The audit command will do the deep parse
// later — state.go is happy with high-level signals.
type ConfigSummary struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	ModTime string `json:"mod_time,omitempty"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
}

// ProbeError records a non-fatal failure so renderers can include it as a
// warning while still showing the rest of the snapshot.
type ProbeError struct {
	Probe   string `json:"probe"`
	Message string `json:"message"`
}

// Collector orchestrates the probes that populate a Snapshot. Each probe
// is overridable so tests can inject deterministic fakes.
type Collector struct {
	HermesDir   string
	Now         func() time.Time
	Hostname    func() (string, error)
	ReadFile    func(path string) ([]byte, error)
	StatFile    func(path string) (os.FileInfo, error)
	ListLogs    func(dir string) ([]os.FileInfo, error)
	LaunchAgent func(ctx context.Context) ([]LaunchAgent, error)
	// RepoLog runs `git -C dir log --oneline -<n>` and returns one commit
	// per output line. Returns an error if dir is not a git checkout (or
	// not a directory at all). Default wires this to the host's `git`
	// binary; tests override to inject deterministic logs.
	RepoLog func(ctx context.Context, dir string, n int) ([]string, error)
	// HomeDir resolves the operator's $HOME so the repo probe can find
	// ~/hermes-setup, ~/owera-cloud, ~/owera-fleet without re-implementing
	// the expansion logic per call site. Default is os.UserHomeDir.
	HomeDir func() (string, error)
}

// Default returns a Collector wired against the live filesystem and the
// host launchctl. hermesDir is the directory under which PINNED_VERSION
// and friends live; pass "" to fall back to $HOME/.hermes.
func Default(hermesDir string) *Collector {
	return &Collector{
		HermesDir: hermesDir,
		Now:       func() time.Time { return time.Now().UTC() },
		Hostname:  os.Hostname,
		ReadFile:  os.ReadFile,
		StatFile:  os.Stat,
		ListLogs: func(dir string) ([]os.FileInfo, error) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			out := make([]os.FileInfo, 0, len(entries))
			for _, e := range entries {
				info, err := e.Info()
				if err != nil {
					continue
				}
				out = append(out, info)
			}
			return out, nil
		},
		LaunchAgent: launchctlList,
		RepoLog:     defaultRepoLog,
		HomeDir:     os.UserHomeDir,
	}
}

// ErrNoHome is returned by Collect when hermesDir is empty and $HOME is unset.
var ErrNoHome = errors.New("state: HOME unset and no hermes dir override")

// Collect runs all probes and returns the assembled Snapshot. Non-fatal
// probe errors are folded into Snapshot.Errors so callers see partial
// success; only completely unrecoverable failures (e.g., HOME unset and
// no override) return an error.
func (c *Collector) Collect(ctx context.Context) (*Snapshot, error) {
	dir := c.HermesDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return nil, ErrNoHome
		}
		dir = filepath.Join(home, ".hermes")
	}
	s := &Snapshot{
		Generated: c.Now(),
		HermesDir: dir,
	}
	host, err := c.Hostname()
	if err == nil {
		s.Hostname = host
	} else {
		s.addError("hostname", err)
	}

	if v, err := c.readFileTrim(filepath.Join(dir, "PINNED_VERSION")); err == nil {
		s.PinnedVersion = v
	} else if !os.IsNotExist(err) {
		s.addError("pinned_version", err)
	}
	if v, err := c.readFileTrim(filepath.Join(dir, "PINNED_VERSION.prev")); err == nil {
		s.PrevVersion = v
	} else if !os.IsNotExist(err) {
		s.addError("prev_version", err)
	}
	if v, err := c.readFileTrim(filepath.Join(dir, "LAST_BACKUP")); err == nil {
		s.LastBackup = v
	} else if !os.IsNotExist(err) {
		s.addError("last_backup", err)
	}

	if data, err := c.ReadFile(filepath.Join(dir, "nodes.txt")); err == nil {
		reg, perr := nodes.Parse(strings.NewReader(string(data)))
		if perr == nil {
			s.Nodes = c.wrapNodes(reg.All(), dir)
		} else {
			s.addError("nodes", perr)
		}
	} else if !os.IsNotExist(err) {
		s.addError("nodes", err)
	}

	s.ConfigSummary = c.collectConfig(dir)

	if c.LaunchAgent != nil {
		agents, err := c.LaunchAgent(ctx)
		if err != nil {
			s.addError("launch_agents", err)
		}
		s.LaunchAgents = annotateAgents(agents)
	}

	s.Productized = buildProductized(s.LaunchAgents)
	s.Repos = c.collectRepos(ctx)

	s.LogSummary = c.collectLogs(filepath.Join(dir, "logs"))

	return s, nil
}

// wrapNodes attaches per-host heartbeat freshness from
// <hermesDir>/heartbeats/<host>.json so the Fleet inventory table can
// emit STATE.md's "heartbeat fresh (<30 s)" column. Missing heartbeat
// files render as "no heartbeat" rather than an error — workers
// provisioned but not yet wired to the bridge are a real operational
// state, not a probe failure.
func (c *Collector) wrapNodes(ns []nodes.Node, hermesDir string) []NodeRow {
	now := c.Now()
	out := make([]NodeRow, 0, len(ns))
	for _, n := range ns {
		row := NodeRow{Node: n}
		path := filepath.Join(hermesDir, "heartbeats", n.Host+".json")
		if info, err := c.StatFile(path); err == nil {
			row.HeartbeatPresent = true
			row.HeartbeatAge = now.Sub(info.ModTime())
			if row.HeartbeatAge < 0 {
				row.HeartbeatAge = 0
			}
		}
		out = append(out, row)
	}
	return out
}

// collectRepos runs git log --oneline -5 against the three canonical
// productized checkouts under $HOME. Missing directories yield a
// RepoState with Present=false; renderers should skip those entries
// rather than emit an empty table for the absent repo.
func (c *Collector) collectRepos(ctx context.Context) []RepoState {
	if c.HomeDir == nil || c.RepoLog == nil {
		return nil
	}
	home, err := c.HomeDir()
	if err != nil || home == "" {
		return nil
	}
	specs := []struct {
		name string
		dir  string
	}{
		{"hermes-setup", filepath.Join(home, "hermes-setup")},
		{"owera-cloud", filepath.Join(home, "owera-cloud")},
		{"owera-fleet", filepath.Join(home, "owera-fleet")},
	}
	out := make([]RepoState, 0, len(specs))
	for _, sp := range specs {
		rs := RepoState{Name: sp.name, Path: sp.dir}
		// StatFile is reused here so tests can drive directory presence
		// without touching the filesystem.
		if _, err := c.StatFile(sp.dir); err == nil {
			rs.Present = true
			if lines, err := c.RepoLog(ctx, sp.dir, 5); err == nil {
				rs.Commits = lines
			}
		}
		out = append(out, rs)
	}
	return out
}

func (c *Collector) readFileTrim(path string) (string, error) {
	data, err := c.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (c *Collector) collectConfig(dir string) ConfigSummary {
	path := filepath.Join(dir, "config.yaml")
	cs := ConfigSummary{Path: path}
	info, err := c.StatFile(path)
	if err != nil {
		return cs
	}
	cs.Bytes = info.Size()
	cs.ModTime = info.ModTime().UTC().Format(time.RFC3339)
	data, err := c.ReadFile(path)
	if err != nil {
		return cs
	}
	cs.Backend = scanYAMLField(data, "backend:")
	cs.Model = scanYAMLField(data, "default:")
	return cs
}

func (c *Collector) collectLogs(dir string) []LogStat {
	entries, err := c.ListLogs(dir)
	if err != nil {
		return nil
	}
	out := make([]LogStat, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		stat := LogStat{
			Name:    name,
			Bytes:   e.Size(),
			ModTime: e.ModTime().UTC(),
		}
		if data, err := c.ReadFile(filepath.Join(dir, name)); err == nil {
			stat.Lines = int64(countLines(data))
		}
		out = append(out, stat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}

func (s *Snapshot) addError(probe string, err error) {
	if err == nil {
		return
	}
	s.Errors = append(s.Errors, ProbeError{Probe: probe, Message: err.Error()})
}

// scanYAMLField is a deliberately shallow scanner: it walks the file
// line-by-line looking for the first line whose trimmed prefix matches
// field, then returns the value after the colon. This is enough for the
// flat top-of-file fields (backend:, default:) and avoids dragging in a
// full YAML parser for the state command. The audit command will use a
// real parser.
func scanYAMLField(data []byte, field string) string {
	for _, raw := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(raw)
		if strings.HasPrefix(trim, field) {
			v := strings.TrimSpace(strings.TrimPrefix(trim, field))
			v = strings.Trim(v, `"'`)
			return v
		}
	}
	return ""
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := strings.Count(string(data), "\n")
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// launchctlList shells out to `launchctl list` and extracts the
// com.hermes.* / com.owera.* / com.cloudflare.cloudflared services along
// with their PIDs and last-exit codes. Returns an empty slice (not nil)
// and an error when launchctl is unavailable so the rest of the snapshot
// still renders via the collector's error-folding path.
//
// `launchctl list` is preferred over `launchctl print gui/$UID` here
// because list has a stable tab-separated three-column layout
// (PID, LastExit, Label) that's straightforward to parse, while print
// produces a nested human-readable dump whose service section is harder
// to extract without false positives.
func launchctlList(ctx context.Context) ([]LaunchAgent, error) {
	cmd := exec.CommandContext(ctx, "launchctl", "list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("launchctl list: %w", err)
	}
	return ParseLaunchctlList(string(out)), nil
}

// ParseLaunchctlList extracts the productized + hermes service rows from
// `launchctl list` output. The file's format is:
//
//	PID	Status	Label
//	-	0	com.foo.bar
//	1234	0	com.baz
//
// (tab-separated, with "-" used when no PID is assigned). We keep only
// labels matching the com.{hermes,owera,cloudflare}.* family — the rest
// of the user's launchd is noise as far as STATE.md is concerned.
//
// Exposed so tests can feed in canned fixtures without shelling out.
func ParseLaunchctlList(out string) []LaunchAgent {
	var agents []LaunchAgent
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "PID") {
			continue
		}
		fields := strings.Fields(trim)
		if len(fields) < 3 {
			continue
		}
		label := fields[len(fields)-1]
		if !isProductizedOrHermesLabel(label) {
			continue
		}
		agents = append(agents, LaunchAgent{
			Label:    label,
			PID:      fields[0],
			LastExit: fields[1],
		})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Label < agents[j].Label })
	return agents
}

// ParseLaunchctlPrint is kept for back-compat with callers that still
// hand in `launchctl print gui/<UID>` output. New code should use
// ParseLaunchctlList against `launchctl list` — print's output is harder
// to parse reliably and was only ever serving the com.hermes.* subset
// of what STATE.md wants today.
func ParseLaunchctlPrint(out string) []LaunchAgent {
	var agents []LaunchAgent
	inServices := false
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "services = {") {
			inServices = true
			continue
		}
		if !inServices {
			continue
		}
		if trim == "}" {
			break
		}
		fields := strings.Fields(trim)
		if len(fields) < 3 {
			continue
		}
		label := fields[len(fields)-1]
		if !isProductizedOrHermesLabel(label) {
			continue
		}
		agents = append(agents, LaunchAgent{
			Label:    label,
			PID:      fields[0],
			LastExit: fields[1],
		})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Label < agents[j].Label })
	return agents
}

func isProductizedOrHermesLabel(label string) bool {
	return strings.HasPrefix(label, "com.hermes.") ||
		strings.HasPrefix(label, "com.owera.") ||
		label == "com.cloudflare.cloudflared"
}

// launchAgentDoc maps each well-known label to its (Mode, Description)
// pair — i.e. the columns STATE.md uses past Label/PID. These are stable
// per-label annotations chosen to match hermes-setup's STATE.md prose;
// when a new agent gets added to the gateway, add an entry here so its
// row gets a human description rather than rendering as "—".
var launchAgentDoc = map[string]struct {
	Mode        string
	Description string
}{
	"com.hermes.backup":           {"Daily 03:15", "scripts/backup-hermes-state.sh → restic to salmonpoke"},
	"com.hermes.backup-worker":    {"Daily 03:05", "scripts/backup-worker-state.sh → rsync workers into gateway tree"},
	"com.hermes.watchdog":         {"Every 120 s", "scripts/heartbeat-watchdog.sh → ntfy.sh on stale >5 min"},
	"com.hermes.logrotate":        {"Daily 03:30", "scripts/rotate-hermes-logs.sh → gzip ~/.hermes/logs/*.jsonl"},
	"com.hermes.heartbeat":        {"Every 60 s", "Worker-side heartbeat touch (installed on workers only)"},
	"com.owera.fleetctl-serve":    {"KeepAlive", "Operator-plane JSON-RPC on 127.0.0.1:9091"},
	"com.owera.snapshot-publish":  {"KeepAlive", "Refreshes ~/.hermes/status/snapshot.json every 30 s"},
	"com.owera.heartbeats-bridge": {"KeepAlive", "SSH-polls workers → writes ~/.hermes/heartbeats/<host>.json"},
	"com.cloudflare.cloudflared":  {"KeepAlive", "cloudflared tunnel run owera-operator-rpc"},
}

// annotateAgents fills Mode/Description for each LaunchAgent from
// launchAgentDoc. Unknown labels are left with empty annotation fields
// (renderer shows "—" so unknown agents are visibly under-documented).
func annotateAgents(in []LaunchAgent) []LaunchAgent {
	out := make([]LaunchAgent, len(in))
	for i, a := range in {
		out[i] = a
		if doc, ok := launchAgentDoc[a.Label]; ok {
			out[i].Mode = doc.Mode
			out[i].Description = doc.Description
		}
	}
	return out
}

// buildProductized constructs the Productized section from the loaded
// LaunchAgents — operator-plane component rows are only emitted for
// agents we actually see in launchctl, so the table doesn't lie about
// a component being live when its agent has been booted out.
//
// CloudPlane fields are hardcoded placeholders today. A follow-up wave
// will read them from $FLY_APP_NAME / $OPERATOR_RPC_URL / a small
// productized-config file when they exist; for now they match the
// values committed to STATE.md so structural parity is preserved.
func buildProductized(agents []LaunchAgent) Productized {
	p := Productized{
		Cloud: CloudPlane{
			App:           "owera-agentic-api (Fly personal org, gru region)",
			Endpoint:      "https://owera-agentic-api.fly.dev",
			HealthCheck:   "both **200** as of last check",
			Auth:          "Clerk JWT (`https://sacred-hen-12.clerk.accounts.dev`) + `owc_*` API keys",
			Billing:       "Stripe live backend (test-mode keys; meter + per-job-fixed dispatcher)",
			OperatorRPC:   "tunnel via `https://internal-rpc.owera.com` (env: `OPERATOR_RPC_URL`)",
			AdminEndpoint: "`POST /v1/admin/tenants[...]/{users,api-keys,cap,stripe-customer,clerk-org,clerk-user}`",
			BootFinger:    "`apiserver: billing=stripe, ledger=tunnel(...), rpc=tunnel(...), auth=clerk(...)`",
		},
	}
	// Component-name -> "where it lives" prose, in the order STATE.md
	// presents it. Only rows whose underlying agent is loaded get
	// emitted; if an operator boots an agent out, its row disappears
	// from the snapshot.
	type comp struct {
		Label string
		Name  string
		Where string
	}
	want := []comp{
		{"com.owera.fleetctl-serve", "`fleetctl serve`", "`127.0.0.1:9091`, JSON-RPC 2.0 (`fleet.SubmitJob`, `fleet.CancelTask`, `fleet.HealthSnapshot`, `fleet.LedgerTail`)"},
		{"com.cloudflare.cloudflared", "Cloudflare Named Tunnel", "routes `internal-rpc.owera.com` → `localhost:9091`"},
		{"com.owera.snapshot-publish", "Snapshot publisher", "`~/.hermes/status/snapshot.json`, refreshed every 30 s"},
		{"com.owera.heartbeats-bridge", "Heartbeats bridge", "SSH-polls workers, writes `~/.hermes/heartbeats/<host>.json` every 10 s"},
	}
	loaded := make(map[string]bool, len(agents))
	for _, a := range agents {
		loaded[a.Label] = true
	}
	for _, w := range want {
		if loaded[w.Label] {
			p.OperatorPlaneOK = append(p.OperatorPlaneOK, OperatorPlane{Component: w.Name, Where: w.Where})
		}
	}
	return p
}

// defaultRepoLog shells out to `git -C <dir> log --oneline -<n>` and
// returns one commit per output line. Errors (non-git directory,
// missing git binary, etc.) surface to the caller — collectRepos
// downgrades them to "no commits" rather than failing the whole
// snapshot.
func defaultRepoLog(ctx context.Context, dir string, n int) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "--oneline", fmt.Sprintf("-%d", n))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", dir, err)
	}
	var lines []string
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimRight(raw, "\r ")
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// Markdown renders the snapshot using internal/report's canonical shape.
//
// The output mirrors the structure of hermes-setup's STATE.md so operators
// reading the regenerated file see the same sections in the same order;
// only the data values + the trailing timestamp differ.
func (s *Snapshot) Markdown() []byte {
	b := report.New("Current operational state", "fleetctl state")
	b.SetClock(func() time.Time { return s.Generated })
	b.Meta("Generated from gateway", emptyOr(s.Hostname, "—"))
	b.Meta("Hermes directory", s.HermesDir)
	b.Meta("Generated by", "fleetctl state (typed replacement for hand-edited STATE.md)")

	b.Section("Fleet inventory")
	if len(s.Nodes) == 0 {
		b.Para("_No nodes registered in `nodes.txt`._")
	} else {
		rows := make([][]string, 0, len(s.Nodes))
		for _, n := range s.Nodes {
			rows = append(rows, []string{n.User, n.Host, heartbeatColumn(n)})
		}
		b.Table([]string{"User", "Host", "Heartbeat"}, rows)
	}

	b.Section("Pinned version")
	b.Bullet(fmt.Sprintf("`PINNED_VERSION`: **%s**", emptyOr(s.PinnedVersion, "—")))
	if s.PrevVersion != "" {
		b.Bullet(fmt.Sprintf("`PINNED_VERSION.prev`: %s", s.PrevVersion))
	} else {
		b.Bullet("`PINNED_VERSION.prev`: (not yet rotated)")
	}

	b.Section("Owera productized layer (cloud + operator plane)")
	b.Para("The two productized repos run alongside hermes-setup. Both deployed to production from `main`.")
	b.Heading(3, "Customer plane — `owera-cloud` on Fly.io")
	b.Table([]string{"Property", "Value"}, [][]string{
		{"App", s.Productized.Cloud.App},
		{"Endpoint", s.Productized.Cloud.Endpoint},
		{"`/healthz`, `/readyz`", s.Productized.Cloud.HealthCheck},
		{"Auth", s.Productized.Cloud.Auth},
		{"Billing", s.Productized.Cloud.Billing},
		{"Operator RPC", s.Productized.Cloud.OperatorRPC},
		{"Admin endpoints", s.Productized.Cloud.AdminEndpoint},
		{"Boot log fingerprint", s.Productized.Cloud.BootFinger},
	})
	b.Heading(3, "Operator plane — `owera-fleet` on `claw3.local`")
	if len(s.Productized.OperatorPlaneOK) == 0 {
		b.Para("_No `com.owera.*` LaunchAgents loaded — operator plane is not running on this gateway._")
	} else {
		rows := make([][]string, 0, len(s.Productized.OperatorPlaneOK))
		for _, op := range s.Productized.OperatorPlaneOK {
			rows = append(rows, []string{op.Component, op.Where})
		}
		b.Table([]string{"Component", "Where it lives"}, rows)
	}

	b.Section("LaunchAgents (gateway, `gui/501`)")
	if len(s.LaunchAgents) == 0 {
		b.Para("_No `com.hermes.*` / `com.owera.*` / `com.cloudflare.cloudflared` services found via `launchctl list`._")
	} else {
		rows := make([][]string, 0, len(s.LaunchAgents))
		for _, la := range s.LaunchAgents {
			rows = append(rows, []string{
				"`" + la.Label + "`",
				emptyOr(la.Mode, "—"),
				pidColumn(la.PID),
				emptyOr(la.Description, "—"),
			})
		}
		b.Table([]string{"Label", "Mode", "PID", "What it runs"}, rows)
	}

	b.Section("Backup status")
	b.Bullet(fmt.Sprintf("Last successful gateway backup (`~/.hermes/LAST_BACKUP`): %s", emptyOr(s.LastBackup, "—")))

	b.Section("Config summary")
	if s.ConfigSummary.Bytes == 0 {
		b.Para(fmt.Sprintf("_`%s` not readable or empty._", s.ConfigSummary.Path))
	} else {
		b.Bullet(fmt.Sprintf("Path: `%s`", s.ConfigSummary.Path))
		b.Bullet(fmt.Sprintf("Size: %d bytes", s.ConfigSummary.Bytes))
		if s.ConfigSummary.ModTime != "" {
			b.Bullet(fmt.Sprintf("Modified: %s", s.ConfigSummary.ModTime))
		}
		if s.ConfigSummary.Backend != "" {
			b.Bullet(fmt.Sprintf("Sandbox backend: `%s`", s.ConfigSummary.Backend))
		}
		if s.ConfigSummary.Model != "" {
			b.Bullet(fmt.Sprintf("Default model: `%s`", s.ConfigSummary.Model))
		}
	}

	b.Section("Recent JSONL logs")
	if len(s.LogSummary) == 0 {
		b.Para("_No JSONL logs found under `logs/`._")
	} else {
		rows := make([][]string, 0, len(s.LogSummary))
		for _, l := range s.LogSummary {
			rows = append(rows, []string{
				l.Name,
				fmt.Sprintf("%d", l.Lines),
				fmt.Sprintf("%d", l.Bytes),
				l.ModTime.Format(time.RFC3339),
			})
		}
		b.Table([]string{"File", "Lines", "Bytes", "Modified"}, rows)
	}

	b.Section("Repository state (3 repos, all on `main`)")
	emitted := 0
	for _, r := range s.Repos {
		if !r.Present {
			continue
		}
		emitted++
		b.Heading(3, fmt.Sprintf("`%s`", r.Name))
		b.Bullet(fmt.Sprintf("Path: `%s`", r.Path))
		if len(r.Commits) == 0 {
			b.Para("_No commits readable (not a git checkout, or git unavailable)._")
			continue
		}
		b.Para(fmt.Sprintf("Last %d commits (most recent first):", len(r.Commits)))
		b.Code("", strings.Join(r.Commits, "\n"))
	}
	if emitted == 0 {
		b.Para("_None of the canonical productized repos are present under $HOME._")
	}

	if len(s.Errors) > 0 {
		b.Section("Probe warnings")
		for _, e := range s.Errors {
			b.Bullet(fmt.Sprintf("`%s`: %s", e.Probe, e.Message))
		}
	}

	return b.Bytes()
}

// heartbeatColumn formats one NodeRow's heartbeat for the inventory
// table. Output mirrors STATE.md's "Status" column wording: "fresh
// (<30s)" when present and recent, "stale (Xm)" when present but old,
// or "no heartbeat" when no file has been seen.
func heartbeatColumn(n NodeRow) string {
	if !n.HeartbeatPresent {
		return "no heartbeat"
	}
	age := n.HeartbeatAge
	switch {
	case age < 30*time.Second:
		return "fresh (<30 s)"
	case age < 5*time.Minute:
		return fmt.Sprintf("fresh (%ds)", int(age.Seconds()))
	default:
		return fmt.Sprintf("stale (%s)", durationHuman(age))
	}
}

// pidColumn renders a launchctl PID for the LaunchAgents table, mapping
// "-" or empty (launchctl's "no live process" marker for cron-style
// agents) to a unicode em-dash so the column reads consistently.
func pidColumn(pid string) string {
	if pid == "" || pid == "-" || pid == "0" {
		return "—"
	}
	return pid
}

// durationHuman renders a duration as STATE.md would write it
// informally — "5m", "2h", "1d". Coarse on purpose: this column is for
// humans glancing at freshness, not metric collection.
func durationHuman(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// JSON renders the snapshot as a pretty-printed JSON document. Useful for
// machine consumers (CI, dashboards) and as a regression fixture for tests.
func (s *Snapshot) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// WriteMarkdown is a convenience that writes Markdown() to w.
func (s *Snapshot) WriteMarkdown(w io.Writer) (int64, error) {
	n, err := w.Write(s.Markdown())
	return int64(n), err
}

func emptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
