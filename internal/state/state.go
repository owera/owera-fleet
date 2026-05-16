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
	Nodes         []nodes.Node  `json:"nodes"`
	LaunchAgents  []LaunchAgent `json:"launch_agents"`
	LogSummary    []LogStat     `json:"log_summary"`
	ConfigSummary ConfigSummary `json:"config_summary"`
	Errors        []ProbeError  `json:"errors,omitempty"`
}

// LaunchAgent is one entry in the gateway's user-domain launchctl list.
type LaunchAgent struct {
	Label    string `json:"label"`
	PID      string `json:"pid,omitempty"`
	LastExit string `json:"last_exit,omitempty"`
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
		LaunchAgent: launchctlPrint,
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
			s.Nodes = reg.All()
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
		s.LaunchAgents = agents
	}

	s.LogSummary = c.collectLogs(filepath.Join(dir, "logs"))

	return s, nil
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

// launchctlPrint shells out to `launchctl print gui/$UID` and extracts the
// com.hermes.* services + their PIDs / last-exit codes. Returns an empty
// slice (not nil) and no error when launchctl is unavailable so the rest
// of the snapshot still renders.
func launchctlPrint(ctx context.Context) ([]LaunchAgent, error) {
	uid := os.Getuid()
	cmd := exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d", uid))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("launchctl print gui/%d: %w", uid, err)
	}
	return ParseLaunchctlPrint(string(out)), nil
}

// ParseLaunchctlPrint extracts com.hermes.* services from launchctl's
// human-readable `print` output. The output has a "services = { ... }"
// block where each line looks like:
//
//	       <pid>   <last_exit>   <label>
//
// Exposed so tests can feed in canned fixtures.
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
		if !strings.HasPrefix(label, "com.hermes.") {
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

	b.Section("Pinned version")
	b.Bullet(fmt.Sprintf("`PINNED_VERSION`: **%s**", emptyOr(s.PinnedVersion, "—")))
	if s.PrevVersion != "" {
		b.Bullet(fmt.Sprintf("`PINNED_VERSION.prev`: %s", s.PrevVersion))
	} else {
		b.Bullet("`PINNED_VERSION.prev`: (not yet rotated)")
	}

	b.Section("Fleet inventory")
	if len(s.Nodes) == 0 {
		b.Para("_No nodes registered in `nodes.txt`._")
	} else {
		rows := make([][]string, 0, len(s.Nodes))
		for _, n := range s.Nodes {
			rows = append(rows, []string{n.User, n.Host})
		}
		b.Table([]string{"User", "Host"}, rows)
	}

	b.Section("LaunchAgents (gateway, user domain)")
	if len(s.LaunchAgents) == 0 {
		b.Para("_No `com.hermes.*` services found via `launchctl print`._")
	} else {
		rows := make([][]string, 0, len(s.LaunchAgents))
		for _, la := range s.LaunchAgents {
			rows = append(rows, []string{la.Label, la.PID, la.LastExit})
		}
		b.Table([]string{"Label", "PID", "Last exit"}, rows)
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

	if len(s.Errors) > 0 {
		b.Section("Probe warnings")
		for _, e := range s.Errors {
			b.Bullet(fmt.Sprintf("`%s`: %s", e.Probe, e.Message))
		}
	}

	return b.Bytes()
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
