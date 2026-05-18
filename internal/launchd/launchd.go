// Package launchd renders LaunchAgent property-list files from embedded
// text/template fragments and drives the macOS `launchctl` lifecycle
// (bootstrap / bootout / print) in a way that is unit-testable.
//
// # Why this exists
//
// hermes-setup shipped five plist templates under scripts/templates/
// (com.hermes.heartbeat/backup/backup-worker/watchdog/logrotate.plist) that
// the installer bash scripts post-processed with sed-substitution markers
// like "{{HERMES_HOME}}" and "{{SCRIPT_PATH}}". owera-fleet replaces that
// install machinery with Go code so:
//
//   - The templating is type-checked (no silent skips when a marker is
//     missing from the sed pipeline).
//   - The launchctl bootstrap/bootout calls are mockable for tests.
//   - The render output is byte-identical to the legacy plists for the
//     production substitutions (the acceptance test in launchd_test.go
//     pins this contract; if you change a template, you must also update
//     the reference plist in hermes-setup or document the deviation in
//     the divergences section of this package doc).
//
// # Template fields
//
// All templates are Go text/template documents. The common fields:
//
//	{{.Label}}      — launchd Label (e.g., "com.hermes.backup")
//	{{.HomeDir}}    — HERMES_HOME (typically the operator's $HOME)
//	{{.RepoDir}}    — hermes-setup checkout (used to find script paths)
//	{{.ScriptPath}} — absolute path to the script the agent runs
//
// Heartbeat additionally needs {{.UserName}} (the worker-side hermes user)
// and {{.NodeLabel}} (the per-worker heartbeat filename). Backup needs
// {{.ResticRepository}} and {{.ResticPasswordCommand}}. Watchdog needs
// {{.AlertURL}}.
//
// # Divergences from the legacy plists
//
// None at the bytes level for the production substitution set. The Go
// templates carry the same indentation (4 spaces), the same XML header
// (DOCTYPE on its own line), and the same trailing newline as the
// hermes-setup sources. The acceptance test in launchd_test.go enforces
// this.
//
// # Cross-arch PATH handling
//
// Every template hardcodes
//
//	PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
//
// in the launchd environment. Both Apple Silicon (`/opt/homebrew/bin`)
// and Intel (`/usr/local/bin`) brew prefixes are included so the same
// rendered plist works on either architecture — bash silently skips
// non-existent directories in PATH, so the unused-prefix entry costs
// nothing at runtime. Templating this via `brew --prefix` at install
// time would actually be worse because it bakes the install-time
// architecture into the plist, breaking when the host's brew prefix
// changes (e.g. Rosetta crossover, migration to a new Mac).
package launchd

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/owera/owera-fleet/internal/log"
)

//go:embed templates/*.tmpl
var embeddedTemplates embed.FS

// templatesDir is the directory inside embeddedTemplates where the .tmpl
// files live. Exposed as a const so tests can list them without re-coding
// the path.
const templatesDir = "templates"

// Template names (matches the file basename, minus the ".plist.tmpl"
// suffix). Use these constants when calling RenderTemplate so a typo
// becomes a compile error rather than a runtime "template not found".
const (
	TemplateHeartbeat        = "heartbeat"
	TemplateBackup           = "backup"
	TemplateBackupWorker     = "backup-worker"
	TemplateWatchdog         = "watchdog"
	TemplateLogrotate        = "logrotate"
	TemplateLogAggregate     = "logaggregate"
	TemplateSnapshotPublish  = "snapshot-publish"
	TemplateHeartbeatsBridge = "heartbeats-bridge"
	// TemplateOweraWatchdog renders the launchd plist for the Go-native
	// staleness watchdog (com.owera.watchdog), which supersedes the
	// legacy hermes-setup bash watchdog (com.hermes.watchdog) by
	// consuming gateway-side ~/.hermes/heartbeats/<host>.json mtimes
	// and firing alerts through internal/alerting.
	TemplateOweraWatchdog = "owera-watchdog"
)

// AllTemplates returns the canonical list of bundled template names in
// the order the bootstrap installer typically applies them. Useful for
// "render everything" sanity checks and for `fleetctl setup-agent --all`
// once that command lands.
func AllTemplates() []string {
	return []string{
		TemplateHeartbeat,
		TemplateBackup,
		TemplateBackupWorker,
		TemplateWatchdog,
		TemplateLogrotate,
		TemplateLogAggregate,
		TemplateSnapshotPublish,
		TemplateHeartbeatsBridge,
		TemplateOweraWatchdog,
	}
}

// Vars holds the substitution variables for a plist render. Fields that
// aren't needed by a given template are simply ignored — text/template
// is happy as long as referenced fields exist on the struct.
//
// Concrete values live in callers (typically fleetctl setup-agent reads
// them from ~/.hermes/config.yaml or operator flags); this struct is the
// shared schema all templates share.
type Vars struct {
	// Label is the launchd Label value, e.g., "com.hermes.backup".
	Label string

	// UID is the macOS UID the agent runs under. Currently unused by any
	// of the bundled templates (UserName is the only user-identifying
	// field, and only heartbeat sets it) but reserved so future
	// per-user-domain agents can pin themselves to a UID without a
	// schema change.
	UID int

	// HomeDir is the operator's $HOME, used wherever HERMES_HOME appears
	// in the legacy plists.
	HomeDir string

	// RepoDir is the hermes-setup checkout root, used to locate the
	// shell scripts the agents invoke.
	RepoDir string

	// ScriptPath is the absolute path to the shell script the agent
	// runs. Callers usually derive it from RepoDir + a relative path.
	ScriptPath string

	// UserName is the macOS user the agent runs as (heartbeat sets this
	// to "hermes" on workers). Leave empty for gateway-side agents that
	// run as the loading user.
	UserName string

	// NodeLabel is the per-worker identifier baked into the heartbeat
	// agent's date-write path (e.g., "claw1.local").
	NodeLabel string

	// ResticRepository is the restic repo URI for the backup agent.
	ResticRepository string

	// ResticPasswordCommand is the restic password-resolution command
	// for the backup agent.
	ResticPasswordCommand string

	// AlertURL is the ntfy.sh (or compatible) endpoint the watchdog
	// posts to when a worker stops heart-beating.
	AlertURL string
}

// Status is the parsed result of `launchctl print <domain>/<label>`.
//
// All fields are best-effort: if launchctl returns a label that's
// loaded-but-never-run, PID will be empty and LastExitCode will be
// "0" (the launchctl default for "never failed"). Loaded reports
// whether the label was found in the domain at all; the rest of the
// fields are only meaningful when Loaded is true.
type Status struct {
	Label        string
	Loaded       bool
	PID          string
	LastExitCode string
	State        string
}

// Manager bundles the dependencies the install/uninstall/status flow
// needs into a single struct so production callers wire them via New()
// and tests substitute fakes per-field.
type Manager struct {
	// FS is the filesystem the plist templates are loaded from. Defaults
	// to the embed.FS baked into this package; tests override to load
	// fixtures.
	FS fs.FS

	// Logger receives one Action call per install / uninstall side
	// effect. Nil means "don't log" (useful in tests).
	Logger *log.Logger

	// Node identifies the host doing the work for the log Event Node
	// field (defaults to "gateway").
	Node string

	// Now stamps Event.Timestamp. Defaults to time.Now.
	Now func() time.Time

	// UID is the macOS UID the bootstrap/bootout commands target. The
	// default is os.Getuid(); tests override to assert command args.
	UID int

	// WriteFile writes the rendered plist to disk. Defaults to
	// os.WriteFile with mode 0o644 (plists must be readable by
	// launchd's loader, which runs in the user's domain).
	WriteFile func(path string, data []byte, perm os.FileMode) error

	// MkdirAll creates parent directories for the plist path. Defaults
	// to os.MkdirAll.
	MkdirAll func(path string, perm os.FileMode) error

	// RemoveFile removes the plist on uninstall. Defaults to os.Remove.
	RemoveFile func(path string) error

	// RunCmd shells out to launchctl. Returns combined stdout+stderr
	// and any exit error. Defaults to a thin wrapper around
	// exec.CommandContext that captures CombinedOutput.
	RunCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// New returns a Manager wired against the live filesystem and the host
// launchctl. Tests construct Manager{} directly with fakes.
func New(logger *log.Logger) *Manager {
	return &Manager{
		FS:         embeddedTemplates,
		Logger:     logger,
		Node:       "gateway",
		Now:        func() time.Time { return time.Now().UTC() },
		UID:        os.Getuid(),
		WriteFile:  os.WriteFile,
		MkdirAll:   os.MkdirAll,
		RemoveFile: os.Remove,
		RunCmd:     defaultRunCmd,
	}
}

func defaultRunCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// ErrTemplateNotFound is returned by RenderTemplate / RenderTemplateFS
// when name doesn't match a bundled .tmpl file.
var ErrTemplateNotFound = errors.New("launchd: template not found")

// ErrUnloaded is returned by Status when the label is not present in
// the user domain. Callers can use errors.Is to distinguish "not loaded"
// from genuine launchctl failure.
var ErrUnloaded = errors.New("launchd: label not loaded")

// RenderTemplate renders the bundled template called name (e.g.,
// "backup") against vars and returns the resulting plist bytes.
//
// Production callers usually call this via a Manager so the embedded
// FS is wired; the package-level function is exposed for callers that
// don't need install/uninstall (e.g., dry-run renders in CI).
func RenderTemplate(name string, vars Vars) ([]byte, error) {
	return renderFromFS(embeddedTemplates, name, vars)
}

// RenderTemplate on Manager honours the injected FS so tests can swap
// in synthetic templates without poking at embeddedTemplates.
func (m *Manager) RenderTemplate(name string, vars Vars) ([]byte, error) {
	src := m.FS
	if src == nil {
		src = embeddedTemplates
	}
	return renderFromFS(src, name, vars)
}

func renderFromFS(src fs.FS, name string, vars Vars) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: empty name", ErrTemplateNotFound)
	}
	path := filepath.Join(templatesDir, name+".plist.tmpl")
	data, err := fs.ReadFile(src, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrTemplateNotFound, name)
		}
		return nil, fmt.Errorf("launchd: read %s: %w", path, err)
	}
	tmpl, err := template.New(name).Option("missingkey=error").Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("launchd: parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("launchd: execute %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// Install writes plist to path (mkdir -p the parent) and bootstraps it
// into the user's launchd domain via `launchctl bootstrap gui/<UID>
// <path>`. The operation is idempotent: bootstrap is a no-op if the
// label is already loaded with the same plist, but launchctl exits
// non-zero with "service already loaded" in that case. The caller
// should pre-check with Status if they need true idempotency; Install
// surfaces the error verbatim so the operator sees what launchctl
// reported.
//
// Side effects are logged via Manager.Logger with phase="launchd",
// action="install:<basename(path)>".
func (m *Manager) Install(ctx context.Context, plist []byte, path string) error {
	if path == "" {
		return errors.New("launchd: empty plist path")
	}
	if len(plist) == 0 {
		return errors.New("launchd: empty plist body")
	}
	start := m.now()
	dir := filepath.Dir(path)
	if err := m.MkdirAll(dir, 0o755); err != nil {
		m.logAction("install:"+filepath.Base(path), log.ResultError, start, "", fmt.Sprintf("mkdir %s: %v", dir, err))
		return fmt.Errorf("launchd: mkdir %s: %w", dir, err)
	}
	if err := m.WriteFile(path, plist, 0o644); err != nil {
		m.logAction("install:"+filepath.Base(path), log.ResultError, start, "", fmt.Sprintf("write %s: %v", path, err))
		return fmt.Errorf("launchd: write %s: %w", path, err)
	}
	domain := fmt.Sprintf("gui/%d", m.UID)
	out, err := m.RunCmd(ctx, "launchctl", "bootstrap", domain, path)
	if err != nil {
		m.logAction("install:"+filepath.Base(path), log.ResultError, start, "", strings.TrimSpace(string(out))+": "+err.Error())
		return fmt.Errorf("launchd: bootstrap %s %s: %w (%s)", domain, path, err, strings.TrimSpace(string(out)))
	}
	m.logAction("install:"+filepath.Base(path), log.ResultOK, start, string(out), "")
	return nil
}

// Uninstall runs `launchctl bootout gui/<UID>/<label>` and then deletes
// the plist at path. If the label isn't loaded, bootout returns a
// non-zero exit; that's surfaced as a warn-level log event but not a
// hard error since the caller's intent (label not loaded, file gone)
// is already achieved if RemoveFile succeeds.
func (m *Manager) Uninstall(ctx context.Context, label, path string) error {
	if label == "" {
		return errors.New("launchd: empty label")
	}
	start := m.now()
	target := fmt.Sprintf("gui/%d/%s", m.UID, label)
	out, err := m.RunCmd(ctx, "launchctl", "bootout", target)
	if err != nil {
		// Bootout-of-unloaded is non-fatal: log as warn and keep going
		// to the file removal step.
		m.logAction("uninstall:"+label, log.ResultWarn, start, string(out), strings.TrimSpace(string(out))+": "+err.Error())
	}
	if path != "" {
		if rerr := m.RemoveFile(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			m.logAction("uninstall:"+label, log.ResultError, start, "", fmt.Sprintf("remove %s: %v", path, rerr))
			return fmt.Errorf("launchd: remove %s: %w", path, rerr)
		}
	}
	if err == nil {
		m.logAction("uninstall:"+label, log.ResultOK, start, string(out), "")
	}
	return nil
}

// Status runs `launchctl print gui/<UID>/<label>` and parses the output
// into a Status. If launchctl reports the label as not loaded (exit 113
// on macOS 13+, or stderr containing "Could not find service"), the
// returned error is ErrUnloaded — callers should errors.Is-check rather
// than treating it as a hard failure.
func (m *Manager) Status(ctx context.Context, label string) (Status, error) {
	if label == "" {
		return Status{}, errors.New("launchd: empty label")
	}
	target := fmt.Sprintf("gui/%d/%s", m.UID, label)
	out, err := m.RunCmd(ctx, "launchctl", "print", target)
	if err != nil {
		if isNotLoaded(out, err) {
			return Status{Label: label, Loaded: false}, ErrUnloaded
		}
		return Status{Label: label}, fmt.Errorf("launchd: print %s: %w (%s)", target, err, strings.TrimSpace(string(out)))
	}
	s := ParseStatus(label, string(out))
	s.Loaded = true
	return s, nil
}

// ParseStatus extracts pid / last-exit-code / state from launchctl's
// human-readable `print` output for a single service. Exposed so tests
// (and future audit code) can feed in canned fixtures.
//
// launchctl print produces lines like:
//
//	pid = 12345
//	last exit code = 0
//	state = running
//
// indented arbitrarily. The parser ignores leading whitespace and
// stops at the first match for each field.
func ParseStatus(label, out string) Status {
	s := Status{Label: label}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case s.PID == "" && strings.HasPrefix(line, "pid ="):
			s.PID = strings.TrimSpace(strings.TrimPrefix(line, "pid ="))
		case s.LastExitCode == "" && strings.HasPrefix(line, "last exit code ="):
			s.LastExitCode = strings.TrimSpace(strings.TrimPrefix(line, "last exit code ="))
		case s.State == "" && strings.HasPrefix(line, "state ="):
			s.State = strings.TrimSpace(strings.TrimPrefix(line, "state ="))
		}
	}
	return s
}

// isNotLoaded inspects the (combined output, error) pair from a
// launchctl print invocation and reports whether it represents a
// "label not loaded" condition. Two signals: stderr containing the
// human-readable "Could not find service" / "service not loaded"
// string, or exit code 113 (the macOS code for "not loaded").
func isNotLoaded(out []byte, err error) bool {
	body := strings.ToLower(string(out))
	if strings.Contains(body, "could not find service") ||
		strings.Contains(body, "not loaded") ||
		strings.Contains(body, "no such process") {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() == 113 {
			return true
		}
	}
	return false
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}

func (m *Manager) logAction(action string, result log.Result, start time.Time, stdout, stderrTail string) {
	if m.Logger == nil {
		return
	}
	end := m.now()
	dur := end.Sub(start).Milliseconds()
	if dur < 0 {
		dur = 0
	}
	// Carry a short stdout tail through StderrTail when there's no
	// stderr — operators reading the JSONL stream want *some* signal
	// from a successful bootstrap (e.g., the empty string launchctl
	// emits on success), so we don't bother truncating to nothing.
	tail := stderrTail
	if tail == "" {
		tail = strings.TrimSpace(stdout)
	}
	_ = m.Logger.Action(log.Event{
		Timestamp:  end,
		Node:       m.Node,
		Phase:      "launchd",
		Action:     action,
		Result:     result,
		DurationMs: dur,
		StderrTail: tail,
	})
}

// PlistPath returns the canonical install path for a label in the
// user's LaunchAgents directory. homeDir overrides $HOME (pass "" to
// use the operator's actual home). The returned path matches what
// hermes-setup's installers used: ~/Library/LaunchAgents/<label>.plist.
func PlistPath(homeDir, label string) string {
	if homeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			homeDir = h
		}
	}
	return filepath.Join(homeDir, "Library", "LaunchAgents", label+".plist")
}

// FormatUID returns "gui/<uid>" — the launchctl domain string for the
// current user. Exposed for callers that build their own launchctl
// invocations alongside this package.
func FormatUID(uid int) string {
	return "gui/" + strconv.Itoa(uid)
}
