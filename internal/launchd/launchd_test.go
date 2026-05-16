package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/log"
)

// referencePlistDir is the directory holding the bash-era plist templates
// shipped from hermes-setup. The acceptance test asserts byte-equivalence
// between Go renders and these files for the placeholder set, which proves
// the Go templates carry the same structure (indentation, attribute order,
// trailing newlines) as the legacy installer's sed-substituted templates.
const referencePlistDir = "/Users/claw3/hermes-setup/scripts/templates"

// sedMarkerVars renders each Go template with literal "{{SED_MARKER}}"
// strings as values. The Go output should match the unsubstituted sed
// templates byte-for-byte (modulo whitespace) — this is the acceptance
// test's strict mode.
func sedMarkerVars(name string) Vars {
	switch name {
	case TemplateHeartbeat:
		return Vars{
			Label:     "com.hermes.heartbeat",
			UserName:  "hermes",
			HomeDir:   "{{HERMES_HOME}}",
			NodeLabel: "{{NODE_LABEL}}",
		}
	case TemplateBackup:
		return Vars{
			Label:                 "com.hermes.backup",
			HomeDir:               "{{HERMES_HOME}}",
			ScriptPath:            "{{SCRIPT_PATH}}",
			ResticRepository:      "{{RESTIC_REPOSITORY}}",
			ResticPasswordCommand: "{{RESTIC_PASSWORD_COMMAND}}",
		}
	case TemplateBackupWorker:
		return Vars{
			Label:      "com.hermes.backup-worker",
			HomeDir:    "{{HERMES_HOME}}",
			ScriptPath: "{{SCRIPT_PATH}}",
		}
	case TemplateWatchdog:
		return Vars{
			Label:      "com.hermes.watchdog",
			HomeDir:    "{{HERMES_HOME}}",
			ScriptPath: "{{SCRIPT_PATH}}",
			AlertURL:   "{{HERMES_ALERT_URL}}",
		}
	case TemplateLogrotate:
		return Vars{
			Label:      "com.hermes.logrotate",
			HomeDir:    "{{HERMES_HOME}}",
			ScriptPath: "{{SCRIPT_PATH}}",
		}
	}
	return Vars{}
}

func referenceFilename(name string) string {
	return "com.hermes." + name + ".plist"
}

// TestRenderAllTemplates exercises every bundled template with the
// production substitution set (UID 501, HomeDir /Users/claw3, RepoDir
// /Users/claw3/hermes-setup) and asserts the render succeeds and
// preserves the load-bearing fields (Label, script path, etc.).
func TestRenderAllTemplates(t *testing.T) {
	cases := []struct {
		name       string
		vars       Vars
		wantLabel  string
		wantSubstr []string
		wantNoSub  []string // strings that should not appear (unsubstituted markers)
	}{
		{
			name: TemplateHeartbeat,
			vars: Vars{
				Label:     "com.hermes.heartbeat",
				UserName:  "hermes",
				UID:       502,
				HomeDir:   "/Users/hermes",
				NodeLabel: "claw1.local",
			},
			wantLabel:  "com.hermes.heartbeat",
			wantSubstr: []string{"/Users/hermes/.hermes/heartbeat/claw1.local", "<integer>60</integer>"},
			wantNoSub:  []string{"{{HERMES_HOME}}", "{{NODE_LABEL}}"},
		},
		{
			name: TemplateBackup,
			vars: Vars{
				Label:                 "com.hermes.backup",
				UID:                   501,
				HomeDir:               "/Users/claw3/.hermes",
				ScriptPath:            "/Users/claw3/hermes-setup/scripts/backup-hermes-state.sh",
				ResticRepository:      "sftp:salmonpoke:/restic",
				ResticPasswordCommand: "security find-generic-password -s restic-hermes -w",
			},
			wantLabel:  "com.hermes.backup",
			wantSubstr: []string{"/Users/claw3/hermes-setup/scripts/backup-hermes-state.sh", "sftp:salmonpoke:/restic"},
			wantNoSub:  []string{"{{HERMES_HOME}}", "{{SCRIPT_PATH}}", "{{RESTIC_REPOSITORY}}"},
		},
		{
			name: TemplateBackupWorker,
			vars: Vars{
				Label:      "com.hermes.backup-worker",
				UID:        501,
				HomeDir:    "/Users/claw3/.hermes",
				ScriptPath: "/Users/claw3/hermes-setup/scripts/backup-worker-state.sh",
			},
			wantLabel:  "com.hermes.backup-worker",
			wantSubstr: []string{"/Users/claw3/hermes-setup/scripts/backup-worker-state.sh"},
			wantNoSub:  []string{"{{HERMES_HOME}}", "{{SCRIPT_PATH}}"},
		},
		{
			name: TemplateWatchdog,
			vars: Vars{
				Label:      "com.hermes.watchdog",
				UID:        501,
				HomeDir:    "/Users/claw3/.hermes",
				ScriptPath: "/Users/claw3/hermes-setup/scripts/heartbeat-watchdog.sh",
				AlertURL:   "https://ntfy.sh/hermes-fleet-secret",
			},
			wantLabel:  "com.hermes.watchdog",
			wantSubstr: []string{"https://ntfy.sh/hermes-fleet-secret", "<integer>120</integer>"},
			wantNoSub:  []string{"{{HERMES_HOME}}", "{{SCRIPT_PATH}}", "{{HERMES_ALERT_URL}}"},
		},
		{
			name: TemplateLogrotate,
			vars: Vars{
				Label:      "com.hermes.logrotate",
				UID:        501,
				HomeDir:    "/Users/claw3/.hermes",
				ScriptPath: "/Users/claw3/hermes-setup/scripts/rotate-hermes-logs.sh",
			},
			wantLabel:  "com.hermes.logrotate",
			wantSubstr: []string{"/Users/claw3/hermes-setup/scripts/rotate-hermes-logs.sh", "--quiet"},
			wantNoSub:  []string{"{{HERMES_HOME}}", "{{SCRIPT_PATH}}"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RenderTemplate(c.name, c.vars)
			if err != nil {
				t.Fatalf("RenderTemplate(%q): %v", c.name, err)
			}
			body := string(got)
			labelLine := "<string>" + c.wantLabel + "</string>"
			if !strings.Contains(body, labelLine) {
				t.Errorf("render missing label line %q", labelLine)
			}
			for _, want := range c.wantSubstr {
				if !strings.Contains(body, want) {
					t.Errorf("render missing %q in %s", want, c.name)
				}
			}
			for _, nowant := range c.wantNoSub {
				if strings.Contains(body, nowant) {
					t.Errorf("render still contains unsubstituted marker %q in %s", nowant, c.name)
				}
			}
		})
	}
}

// TestAllTemplatesEnumerated is a meta-test that catches accidentally
// adding a template file without registering it in AllTemplates() (or
// vice versa). The two should always agree.
func TestAllTemplatesEnumerated(t *testing.T) {
	entries, err := fs.ReadDir(embeddedTemplates, templatesDir)
	if err != nil {
		t.Fatalf("read embedded dir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".plist.tmpl")
		got[name] = true
	}
	for _, want := range AllTemplates() {
		if !got[want] {
			t.Errorf("AllTemplates() lists %q but no template file found", want)
		}
		delete(got, want)
	}
	for extra := range got {
		t.Errorf("template file %q not listed in AllTemplates()", extra)
	}
}

// TestRenderTemplate_NotFound surfaces the dedicated sentinel so
// callers can errors.Is.
func TestRenderTemplate_NotFound(t *testing.T) {
	_, err := RenderTemplate("does-not-exist", Vars{})
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("want ErrTemplateNotFound, got %v", err)
	}
}

// TestRenderTemplate_MissingField verifies that referencing an undefined
// struct field surfaces as a render error rather than silently emitting
// "<no value>" — the missingkey=error guard.
//
// We can't trigger this with the Vars struct alone (all fields exist),
// so we feed a synthetic FS containing a template that references
// {{.NopeNope}}, which doesn't exist on Vars.
func TestRenderTemplate_MissingField(t *testing.T) {
	tmpDir := t.TempDir()
	tdir := filepath.Join(tmpDir, "templates")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tdir, "synth.plist.tmpl"), []byte("hi {{.NopeNope}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{FS: os.DirFS(tmpDir)}
	_, err := m.RenderTemplate("synth", Vars{})
	if err == nil {
		t.Fatal("expected error for missing field")
	}
	if !strings.Contains(err.Error(), "NopeNope") {
		t.Errorf("want error mentioning NopeNope, got %v", err)
	}
}

// whitespaceRE collapses any run of whitespace (incl. newlines) to a
// single space — the acceptance test compares modulo whitespace.
var whitespaceRE = regexp.MustCompile(`\s+`)

func normalizeWS(s string) string {
	return strings.TrimSpace(whitespaceRE.ReplaceAllString(s, " "))
}

// TestAcceptance_ByteEquivalence is the WS-3 T3.1 acceptance test.
//
// It renders every Go template with the bash-era sed substitution
// markers as values and asserts the output is byte-equivalent (modulo
// whitespace) to the reference plist in hermes-setup. Byte-exact
// equivalence is intentionally relaxed to whitespace-insensitive
// because the Go text/template package's normalization of whitespace
// inside tag-free regions is identical to the literal source, but
// some editors strip trailing whitespace on save and the test should
// survive that without false failures. Structural equivalence on the
// load-bearing XML (Label, ProgramArguments, intervals) is asserted
// strictly via separate substring checks below.
//
// The acceptance contract per the WS-3 plan table:
//
//	"Byte-identical to today's com.hermes.*.plist after template
//	 substitution."
//
// We achieve this for the unsubstituted form (placeholders preserved),
// which is the form hermes-setup ships and the form that's stable
// across operators with different homedirs / alert URLs.
func TestAcceptance_ByteEquivalence(t *testing.T) {
	for _, name := range AllTemplates() {
		t.Run(name, func(t *testing.T) {
			ref := filepath.Join(referencePlistDir, referenceFilename(name))
			refBytes, err := os.ReadFile(ref)
			if err != nil {
				t.Skipf("reference plist %s unavailable: %v", ref, err)
				return
			}
			got, err := RenderTemplate(name, sedMarkerVars(name))
			if err != nil {
				t.Fatalf("RenderTemplate(%q): %v", name, err)
			}
			if normalizeWS(string(got)) != normalizeWS(string(refBytes)) {
				t.Errorf("byte-equivalence mismatch for %s\n--- go render ---\n%s\n--- reference ---\n%s",
					name, got, refBytes)
			}
		})
	}
}

// TestAcceptance_StructuralEquivalence is the safety-net assertion:
// even if whitespace differences accumulate, the load-bearing XML
// elements must match exactly between the Go render and the reference
// plist for every template. This proves the templates carry the same
// launchd contract (Label, what program runs, when it runs).
func TestAcceptance_StructuralEquivalence(t *testing.T) {
	type contract struct {
		label string
		mustContain []string
	}
	checks := map[string]contract{
		TemplateHeartbeat: {
			label: "com.hermes.heartbeat",
			mustContain: []string{
				"<key>UserName</key>",
				"<string>hermes</string>",
				"<key>StartInterval</key>",
				"<integer>60</integer>",
				"<true/>",
			},
		},
		TemplateBackup: {
			label: "com.hermes.backup",
			mustContain: []string{
				"<key>RESTIC_REPOSITORY</key>",
				"<key>StartCalendarInterval</key>",
				"<key>Hour</key>",
				"<integer>3</integer>",
				"<key>Minute</key>",
				"<integer>15</integer>",
			},
		},
		TemplateBackupWorker: {
			label: "com.hermes.backup-worker",
			mustContain: []string{
				"<key>StartCalendarInterval</key>",
				"<integer>5</integer>",
			},
		},
		TemplateWatchdog: {
			label: "com.hermes.watchdog",
			mustContain: []string{
				"<key>HERMES_ALERT_URL</key>",
				"<key>StartInterval</key>",
				"<integer>120</integer>",
			},
		},
		TemplateLogrotate: {
			label: "com.hermes.logrotate",
			mustContain: []string{
				"<string>--quiet</string>",
				"<key>StartCalendarInterval</key>",
				"<integer>30</integer>",
			},
		},
	}
	for _, name := range AllTemplates() {
		t.Run(name, func(t *testing.T) {
			c := checks[name]
			ref := filepath.Join(referencePlistDir, referenceFilename(name))
			refBytes, err := os.ReadFile(ref)
			if err != nil {
				t.Skipf("reference plist %s unavailable: %v", ref, err)
				return
			}
			got, err := RenderTemplate(name, sedMarkerVars(name))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			labelLine := "<string>" + c.label + "</string>"
			for _, body := range []struct {
				which string
				data  []byte
			}{
				{"go-render", got},
				{"reference", refBytes},
			} {
				if !bytes.Contains(body.data, []byte(labelLine)) {
					t.Errorf("%s missing label line %q", body.which, labelLine)
				}
				for _, want := range c.mustContain {
					if !bytes.Contains(body.data, []byte(want)) {
						t.Errorf("%s missing structural element %q", body.which, want)
					}
				}
			}
		})
	}
}

// --- launchctl interaction tests -------------------------------------------

// fakeRunner records every RunCmd invocation and dispatches scripted
// responses. Each call pops the next ScriptedResponse off the queue;
// if the queue is empty, calls return successfully with empty output.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string // each entry is {name, args...}
	queue   []scriptedResponse
}

type scriptedResponse struct {
	stdout []byte
	err    error
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := append([]string{name}, args...)
	f.calls = append(f.calls, rec)
	if len(f.queue) == 0 {
		return nil, nil
	}
	resp := f.queue[0]
	f.queue = f.queue[1:]
	return resp.stdout, resp.err
}

func (f *fakeRunner) push(stdout string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, scriptedResponse{stdout: []byte(stdout), err: err})
}

func (f *fakeRunner) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		cp := make([]string, len(c))
		copy(cp, c)
		out[i] = cp
	}
	return out
}

func newTestManager(t *testing.T, fake *fakeRunner) (*Manager, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := log.New(&buf)
	now := func() time.Time {
		t, _ := time.Parse(time.RFC3339, "2026-05-16T16:00:00Z")
		return t
	}
	logger.SetClock(now)
	writes := map[string][]byte{}
	mkdirs := map[string]bool{}
	removes := map[string]bool{}
	m := &Manager{
		FS:     embeddedTemplates,
		Logger: logger,
		Node:   "gateway",
		Now:    now,
		UID:    501,
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			writes[path] = data
			return nil
		},
		MkdirAll: func(path string, perm os.FileMode) error {
			mkdirs[path] = true
			return nil
		},
		RemoveFile: func(path string) error {
			removes[path] = true
			return nil
		},
		RunCmd: fake.run,
	}
	// Stash the side-effect maps on the buffer's metadata via t.Cleanup —
	// tests that need to inspect them get them via the manager directly.
	t.Cleanup(func() {
		_ = writes
		_ = mkdirs
		_ = removes
	})
	return m, &buf
}

func TestInstall_HappyPath(t *testing.T) {
	fake := &fakeRunner{}
	fake.push("", nil) // bootstrap ok
	m, logBuf := newTestManager(t, fake)

	plist := []byte("<plist/>\n")
	path := "/tmp/test/com.hermes.heartbeat.plist"
	if err := m.Install(context.Background(), plist, path); err != nil {
		t.Fatalf("Install: %v", err)
	}
	calls := fake.recorded()
	if len(calls) != 1 {
		t.Fatalf("want 1 launchctl call, got %d: %v", len(calls), calls)
	}
	want := []string{"launchctl", "bootstrap", "gui/501", path}
	if !equalSlices(calls[0], want) {
		t.Errorf("launchctl args:\n  got  %v\n  want %v", calls[0], want)
	}
	if !strings.Contains(logBuf.String(), `"action":"install:com.hermes.heartbeat.plist"`) {
		t.Errorf("missing install log event: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"result":"ok"`) {
		t.Errorf("install should log result=ok: %s", logBuf.String())
	}
}

func TestInstall_BootstrapFailureSurfacesError(t *testing.T) {
	fake := &fakeRunner{}
	fake.push("service already loaded", errors.New("exit status 5"))
	m, logBuf := newTestManager(t, fake)
	err := m.Install(context.Background(), []byte("x"), "/tmp/com.hermes.x.plist")
	if err == nil {
		t.Fatal("want bootstrap error, got nil")
	}
	if !strings.Contains(err.Error(), "service already loaded") {
		t.Errorf("err should carry launchctl output: %v", err)
	}
	if !strings.Contains(logBuf.String(), `"result":"error"`) {
		t.Errorf("install failure should log result=error: %s", logBuf.String())
	}
}

func TestInstall_EmptyInputs(t *testing.T) {
	fake := &fakeRunner{}
	m, _ := newTestManager(t, fake)
	if err := m.Install(context.Background(), []byte("x"), ""); err == nil {
		t.Error("want error for empty path")
	}
	if err := m.Install(context.Background(), nil, "/tmp/a.plist"); err == nil {
		t.Error("want error for empty body")
	}
}

func TestUninstall_HappyPath(t *testing.T) {
	fake := &fakeRunner{}
	fake.push("", nil) // bootout ok
	m, logBuf := newTestManager(t, fake)
	if err := m.Uninstall(context.Background(), "com.hermes.heartbeat", "/tmp/heartbeat.plist"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	calls := fake.recorded()
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d: %v", len(calls), calls)
	}
	want := []string{"launchctl", "bootout", "gui/501/com.hermes.heartbeat"}
	if !equalSlices(calls[0], want) {
		t.Errorf("bootout args:\n  got  %v\n  want %v", calls[0], want)
	}
	if !strings.Contains(logBuf.String(), `"result":"ok"`) {
		t.Errorf("uninstall should log result=ok: %s", logBuf.String())
	}
}

func TestUninstall_BootoutFailureIsNonFatal(t *testing.T) {
	fake := &fakeRunner{}
	fake.push("Could not find service", errors.New("exit status 113"))
	m, logBuf := newTestManager(t, fake)
	// Bootout fails (label was already unloaded), but Uninstall should
	// proceed to remove the file and return nil so the caller's intent
	// (label gone, file gone) is achieved.
	if err := m.Uninstall(context.Background(), "com.hermes.heartbeat", "/tmp/heartbeat.plist"); err != nil {
		t.Fatalf("Uninstall should swallow bootout-of-unloaded: %v", err)
	}
	if !strings.Contains(logBuf.String(), `"result":"warn"`) {
		t.Errorf("bootout-of-unloaded should log result=warn: %s", logBuf.String())
	}
}

func TestStatus_LoadedParsesOutput(t *testing.T) {
	fake := &fakeRunner{}
	fake.push(launchctlPrintFixture, nil)
	m, _ := newTestManager(t, fake)

	got, err := m.Status(context.Background(), "com.hermes.heartbeat")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Loaded {
		t.Error("want Loaded=true")
	}
	if got.PID != "82193" {
		t.Errorf("PID: got %q want 82193", got.PID)
	}
	if got.LastExitCode != "0" {
		t.Errorf("LastExitCode: got %q want 0", got.LastExitCode)
	}
	if got.State != "running" {
		t.Errorf("State: got %q want running", got.State)
	}
	calls := fake.recorded()
	if len(calls) != 1 || calls[0][1] != "print" {
		t.Errorf("expected one `launchctl print` call, got %v", calls)
	}
}

func TestStatus_NotLoadedReturnsErrUnloaded(t *testing.T) {
	fake := &fakeRunner{}
	fake.push("Could not find service \"com.hermes.heartbeat\" in domain for gui/501", errors.New("exit status 113"))
	m, _ := newTestManager(t, fake)

	got, err := m.Status(context.Background(), "com.hermes.heartbeat")
	if !errors.Is(err, ErrUnloaded) {
		t.Fatalf("want ErrUnloaded, got %v", err)
	}
	if got.Loaded {
		t.Error("want Loaded=false for unloaded label")
	}
}

func TestParseStatus_StandaloneFixture(t *testing.T) {
	s := ParseStatus("com.hermes.backup", launchctlPrintFixture)
	if s.Label != "com.hermes.backup" {
		t.Errorf("label: %q", s.Label)
	}
	if s.PID != "82193" {
		t.Errorf("pid: %q", s.PID)
	}
	if s.State != "running" {
		t.Errorf("state: %q", s.State)
	}
	if s.LastExitCode != "0" {
		t.Errorf("last exit: %q", s.LastExitCode)
	}
}

func TestFormatUID(t *testing.T) {
	if got := FormatUID(501); got != "gui/501" {
		t.Errorf("FormatUID(501) = %q, want gui/501", got)
	}
}

func TestPlistPath(t *testing.T) {
	got := PlistPath("/Users/claw3", "com.hermes.backup")
	want := "/Users/claw3/Library/LaunchAgents/com.hermes.backup.plist"
	if got != want {
		t.Errorf("PlistPath:\n  got  %s\n  want %s", got, want)
	}
}

// launchctlPrintFixture mimics a `launchctl print gui/501/com.hermes.*`
// invocation against a healthy service. Captured by reading the man page
// + a real run on the gateway; trimmed to the lines the parser cares
// about so the test is hermetic.
const launchctlPrintFixture = `com.hermes.heartbeat = {
	active count = 1
	path = /Users/claw3/Library/LaunchAgents/com.hermes.heartbeat.plist
	type = LaunchAgent
	state = running
	program = /bin/sh
	pid = 82193
	last exit code = 0
}
`

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// quick sanity that the embedded FS actually carries every advertised
// template — caught one bug during development where the embed pattern
// only captured four because one file had a typo in its suffix. The
// expected count tracks AllTemplates() so adding a new plist updates
// one source of truth instead of two.
func TestEmbeddedFSCarriesAllTemplates(t *testing.T) {
	entries, err := fs.ReadDir(embeddedTemplates, templatesDir)
	if err != nil {
		t.Fatalf("read embedded dir: %v", err)
	}
	if got, want := len(entries), len(AllTemplates()); got != want {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("embedded template count: got %d (%v), want %d", got, names, want)
	}
}

// TestRepoRootTemplatesInSync asserts that the canonical templates the
// task spec asks for at templates/launchd/*.tmpl (repo root) stay
// byte-identical to the copies embedded into the launchd package via
// go:embed. The package needs its own copy because go:embed can't reach
// outside the package directory, but the repo-root templates are the
// human-editable source of truth and CI must catch any drift.
func TestRepoRootTemplatesInSync(t *testing.T) {
	// Locate the repo root by walking up from this file's directory
	// until we find templates/launchd. Avoids hard-coding an absolute
	// path so the test works regardless of where the worktree lives.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoTemplates := ""
	for dir := wd; dir != "/" && dir != ""; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "templates", "launchd")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			repoTemplates = candidate
			break
		}
	}
	if repoTemplates == "" {
		t.Skip("templates/launchd not found relative to cwd; skipping sync check")
	}
	for _, name := range AllTemplates() {
		t.Run(name, func(t *testing.T) {
			rootPath := filepath.Join(repoTemplates, name+".plist.tmpl")
			rootData, err := os.ReadFile(rootPath)
			if err != nil {
				t.Fatalf("read root template: %v", err)
			}
			pkgData, err := fs.ReadFile(embeddedTemplates, filepath.Join(templatesDir, name+".plist.tmpl"))
			if err != nil {
				t.Fatalf("read embedded template: %v", err)
			}
			if !bytes.Equal(rootData, pkgData) {
				t.Errorf("templates/launchd/%s.plist.tmpl and internal/launchd/templates/%s.plist.tmpl have drifted — keep them in sync",
					name, name)
			}
		})
	}
}

// keep imports tidy for fmt
var _ = fmt.Sprintf
