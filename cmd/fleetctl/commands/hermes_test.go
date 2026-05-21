package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/log"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

// fakeHermesClient implements RemoteClientWith so we can drive both the
// audit path (Run) and the sync path (RunWith) with a single fake.
type fakeHermesClient struct {
	statusOut    string
	statusExit   int
	statusErr    error
	syncOut      string
	syncErr      error
	syncExit     int
	gotStdinPath string
	stdinSeen    string
	closed       bool
}

func (f *fakeHermesClient) Run(_ context.Context, cmd string) (osh.Result, error) {
	// audit + post-sync verify path runs hermesStatusCmd
	if strings.Contains(cmd, "hermes status") {
		return osh.Result{Stdout: f.statusOut, ExitCode: f.statusExit}, f.statusErr
	}
	return osh.Result{}, nil
}

func (f *fakeHermesClient) RunWith(_ context.Context, _ string, opts osh.RunOpts) (osh.Result, error) {
	if opts.Stdin != nil {
		b, _ := io.ReadAll(opts.Stdin)
		f.stdinSeen = string(b)
	}
	return osh.Result{Stdout: f.syncOut, ExitCode: f.syncExit}, f.syncErr
}

func (f *fakeHermesClient) Close() error { f.closed = true; return nil }

// fakeHermesDialer hands out a single fakeHermesClient per Dial.
type fakeHermesDialer struct {
	dialErr error
	client  *fakeHermesClient
}

func (f *fakeHermesDialer) Dial(_ context.Context, _ osh.Target, _ ...osh.Option) (RemoteClient, error) {
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return f.client, nil
}

func withHermesTestNodes(t *testing.T, contents string) func() {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	prev := hermesNodesPath
	hermesNodesPath = path
	return func() { hermesNodesPath = prev }
}

func withHermesTestDir(t *testing.T, envContent, configContent string) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	if envContent != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envContent), 0o600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
	}
	if configContent != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configContent), 0o600); err != nil {
			t.Fatalf("write config.yaml: %v", err)
		}
	}
	prev := hermesHermesDir
	hermesHermesDir = dir
	return dir, func() { hermesHermesDir = prev }
}

func withHermesTestLog(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	prev := hermesLogPath
	hermesLogPath = filepath.Join(dir, "hermes.jsonl")
	return func() { hermesLogPath = prev }
}

// ── ParseHermesStatus ─────────────────────────────────────────────────

func TestParseHermesStatus(t *testing.T) {
	configured := `
◆ Environment
  Project:      /opt/homebrew/Cellar/hermes-agent/...
  Model:        nvidia/nemotron-3-super-120b-a12b:free
  Provider:     OpenRouter

◆ API Keys
  OpenRouter    ✓ sk-o...da69
  OpenAI        ✗ (not set)
  Anthropic     ✓ sk-a...QQAA
`
	bootstrappedButMissingKey := `
◆ Environment
  Model:        nvidia/nemotron-3-super-120b-a12b:free
  Provider:     OpenRouter

◆ API Keys
  OpenRouter    ✗ (not set)
  OpenAI        ✗ (not set)
`
	unavailable := `HERMES_UNAVAILABLE
`
	cases := []struct {
		name           string
		out            string
		wantConfigured bool
		wantNeedsSetup bool
		wantModel      string
		wantProvider   string
		wantKeys       []string
	}{
		{"fully configured", configured, true, false,
			"nvidia/nemotron-3-super-120b-a12b:free", "OpenRouter",
			[]string{"Anthropic", "OpenRouter"}},
		{"bootstrapped, key missing", bootstrappedButMissingKey, false, true,
			"nvidia/nemotron-3-super-120b-a12b:free", "OpenRouter",
			[]string{}},
		{"hermes binary missing", unavailable, false, true, "", "", []string{}},
		{"empty input", "", false, false, "", "", []string{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseHermesStatus(c.out)
			if got.Configured != c.wantConfigured {
				t.Errorf("Configured = %v, want %v", got.Configured, c.wantConfigured)
			}
			if got.NeedsSetup != c.wantNeedsSetup {
				t.Errorf("NeedsSetup = %v, want %v", got.NeedsSetup, c.wantNeedsSetup)
			}
			if got.Model != c.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, c.wantModel)
			}
			if got.Provider != c.wantProvider {
				t.Errorf("Provider = %q, want %q", got.Provider, c.wantProvider)
			}
			if !equalStrSlices(got.KeysPresent, c.wantKeys) {
				t.Errorf("KeysPresent = %v, want %v", got.KeysPresent, c.wantKeys)
			}
		})
	}
}

// ── readGatewayAuthEnv ───────────────────────────────────────────────

func TestReadGatewayAuthEnv(t *testing.T) {
	dir := t.TempDir()
	content := `# gateway secrets
OPENROUTER_API_KEY=sk-or-123
ANTHROPIC_API_KEY="sk-ant-456"
# not an auth key
TERMINAL_TIMEOUT=30
EMPTY_KEY=
INVALID line
SOMETHING_RANDOM=abc
ANTHROPIC_TOKEN='token-789'
`
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	got, err := readGatewayAuthEnv(dir)
	if err != nil {
		t.Fatalf("readGatewayAuthEnv: %v", err)
	}
	want := map[string]string{
		"OPENROUTER_API_KEY": "sk-or-123",
		"ANTHROPIC_API_KEY":  "sk-ant-456",
		"ANTHROPIC_TOKEN":    "token-789",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d (got: %v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%s] = %q, want %q", k, got[k], v)
		}
	}
	// Negative: TERMINAL_TIMEOUT must NOT be returned even though it's in .env
	if _, ok := got["TERMINAL_TIMEOUT"]; ok {
		t.Errorf("TERMINAL_TIMEOUT leaked into auth env — allowlist is bypassed")
	}
	if _, ok := got["EMPTY_KEY"]; ok {
		t.Errorf("empty value should be dropped")
	}
}

// ── readGatewayModel ─────────────────────────────────────────────────

func TestReadGatewayModel(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"plain inline", "model: nvidia/nemotron-3-super-120b-a12b:free\nproviders: {}\n",
			"nvidia/nemotron-3-super-120b-a12b:free"},
		{"nested block (gateway shape)",
			"model:\n  default: nvidia/nemotron-3-super-120b-a12b:free\n  fallback: anthropic/claude-haiku-4-5\nproviders: {}\n",
			"nvidia/nemotron-3-super-120b-a12b:free"},
		{"nested block but no default",
			"model:\n  fallback: anthropic/claude-haiku-4-5\nproviders: {}\n",
			""},
		{"double-quoted", `model: "anthropic/claude-haiku-4-5"` + "\n",
			"anthropic/claude-haiku-4-5"},
		{"single-quoted", `model: 'gpt-4o-mini'` + "\n", "gpt-4o-mini"},
		{"missing", "providers: {}\n", ""},
		{"indented (must not match — top-level only)", "  model: nope\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
				[]byte(c.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := readGatewayModel(dir)
			if err != nil {
				t.Fatalf("readGatewayModel: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ── shellSingleQuote ─────────────────────────────────────────────────

func TestShellSingleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "'plain'"},
		{"with spaces", "'with spaces'"},
		{`he said "hi"`, `'he said "hi"'`},
		{"it's", `'it'"'"'s'`},
		{"a;rm -rf / `whoami`", "'a;rm -rf / `whoami`'"},
	}
	for _, c := range cases {
		got := shellSingleQuote(c.in)
		if got != c.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── buildAuthPayload ─────────────────────────────────────────────────

func TestBuildAuthPayload(t *testing.T) {
	auth := map[string]string{
		"OPENROUTER_API_KEY": "sk-or-1",
		"ANTHROPIC_TOKEN":    "tok-2",
	}
	got := buildAuthPayload(auth)
	// Should be NUL-terminated, sorted by key.
	parts := strings.Split(got, "\x00")
	// last element will be empty due to trailing NUL
	if len(parts) != 3 || parts[len(parts)-1] != "" {
		t.Fatalf("expected 2 records + trailing NUL, got %d parts: %q", len(parts), parts)
	}
	if parts[0] != "ANTHROPIC_TOKEN=tok-2" {
		t.Errorf("record[0] = %q, want sorted-first entry", parts[0])
	}
	if parts[1] != "OPENROUTER_API_KEY=sk-or-1" {
		t.Errorf("record[1] = %q", parts[1])
	}
}

// ── end-to-end audit RunE drive ──────────────────────────────────────

func TestRunHermesAudit_Configured(t *testing.T) {
	resetHermesFlags()
	defer resetHermesFlags()

	cleanup := withHermesTestNodes(t, "hermes@claw1.local\n")
	defer cleanup()
	defer withHermesTestLog(t)()
	hermesAll = true
	hermesJSON = true

	fake := &fakeHermesClient{
		statusOut: `Model: nvidia/nemotron-3-super-120b-a12b:free
Provider: OpenRouter
◆ API Keys
  OpenRouter    ✓ sk-o...da69
`,
		statusExit: 0,
	}
	prevDialer := newDialer
	newDialer = func(_ ...osh.Option) Dialer { return &fakeHermesDialer{client: fake} }
	defer func() { newDialer = prevDialer }()

	prevLog := openLogger
	openLogger = func(_ string) (*log.Logger, error) {
		return log.Open(filepath.Join(t.TempDir(), "h.jsonl"))
	}
	defer func() { openLogger = prevLog }()

	var buf bytes.Buffer
	hermesAuditCmd.SetOut(&buf)
	defer hermesAuditCmd.SetOut(nil)

	if err := runHermesAudit(hermesAuditCmd, nil); err != nil {
		t.Fatalf("runHermesAudit: %v", err)
	}
	var results []HermesStatus
	if err := json.Unmarshal(buf.Bytes(), &results); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, buf.String())
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	got := results[0]
	if !got.Configured {
		t.Errorf("Configured = false, want true (raw=%q)", got.Raw)
	}
	if got.Provider != "OpenRouter" || got.Model != "nvidia/nemotron-3-super-120b-a12b:free" {
		t.Errorf("unexpected fields: %+v", got)
	}
	if !fake.closed {
		t.Errorf("client.Close was not called")
	}
}

func TestRunHermesAudit_NeedsSetup(t *testing.T) {
	resetHermesFlags()
	defer resetHermesFlags()
	cleanup := withHermesTestNodes(t, "hermes@claw1.local\n")
	defer cleanup()
	defer withHermesTestLog(t)()
	hermesAll = true
	hermesJSON = true

	fake := &fakeHermesClient{
		statusOut: `Model: nvidia/nemotron-3-super-120b-a12b:free
Provider: OpenRouter
◆ API Keys
  OpenRouter    ✗ (not set)
`,
	}
	prev := newDialer
	newDialer = func(_ ...osh.Option) Dialer { return &fakeHermesDialer{client: fake} }
	defer func() { newDialer = prev }()
	prevLog := openLogger
	openLogger = func(_ string) (*log.Logger, error) {
		return log.Open(filepath.Join(t.TempDir(), "h.jsonl"))
	}
	defer func() { openLogger = prevLog }()

	var buf bytes.Buffer
	hermesAuditCmd.SetOut(&buf)
	defer hermesAuditCmd.SetOut(nil)
	if err := runHermesAudit(hermesAuditCmd, nil); err != nil {
		t.Fatalf("audit: %v", err)
	}
	var results []HermesStatus
	if err := json.Unmarshal(buf.Bytes(), &results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !results[0].NeedsSetup {
		t.Errorf("NeedsSetup=false, want true (provider set but key missing)")
	}
}

// ── end-to-end sync RunE drive ───────────────────────────────────────

func TestRunHermesSync_PushesStdinPayload(t *testing.T) {
	resetHermesFlags()
	defer resetHermesFlags()

	_, dirCleanup := withHermesTestDir(t,
		"OPENROUTER_API_KEY=sk-or-test\nANTHROPIC_TOKEN=tok-test\nTERMINAL_TIMEOUT=30\n",
		"model: nvidia/nemotron-3-super-120b-a12b:free\nproviders: {}\n")
	defer dirCleanup()

	cleanup := withHermesTestNodes(t, "hermes@claw1.local\n")
	defer cleanup()
	defer withHermesTestLog(t)()
	hermesAll = true
	hermesJSON = true

	fake := &fakeHermesClient{
		statusOut: `Model: nvidia/nemotron-3-super-120b-a12b:free
Provider: OpenRouter
◆ API Keys
  OpenRouter    ✓ sk-o...te`,
		statusExit: 0,
		syncOut:    "HERMES_SYNC_OK\n",
		syncExit:   0,
	}
	prev := newDialer
	newDialer = func(_ ...osh.Option) Dialer { return &fakeHermesDialer{client: fake} }
	defer func() { newDialer = prev }()
	prevLog := openLogger
	openLogger = func(_ string) (*log.Logger, error) {
		return log.Open(filepath.Join(t.TempDir(), "h.jsonl"))
	}
	defer func() { openLogger = prevLog }()

	var buf bytes.Buffer
	hermesSyncCmd.SetOut(&buf)
	defer hermesSyncCmd.SetOut(nil)

	if err := runHermesSync(hermesSyncCmd, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The stdin payload must contain BOTH allow-listed keys and NOT contain
	// the terminal config that was in .env but isn't an auth key.
	if !strings.Contains(fake.stdinSeen, "OPENROUTER_API_KEY=sk-or-test") {
		t.Errorf("stdin missing OPENROUTER_API_KEY; got: %q", fake.stdinSeen)
	}
	if !strings.Contains(fake.stdinSeen, "ANTHROPIC_TOKEN=tok-test") {
		t.Errorf("stdin missing ANTHROPIC_TOKEN; got: %q", fake.stdinSeen)
	}
	if strings.Contains(fake.stdinSeen, "TERMINAL_TIMEOUT") {
		t.Errorf("stdin leaked non-auth key TERMINAL_TIMEOUT; got: %q", fake.stdinSeen)
	}

	var results []SyncResult
	if err := json.Unmarshal(buf.Bytes(), &results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].ExitCode != 0 {
		t.Fatalf("unexpected results: %+v", results)
	}
	if results[0].ModelApplied != "nvidia/nemotron-3-super-120b-a12b:free" {
		t.Errorf("ModelApplied = %q, want gateway model", results[0].ModelApplied)
	}
	if !equalStrSlices(results[0].KeysPushed,
		[]string{"ANTHROPIC_TOKEN", "OPENROUTER_API_KEY"}) {
		t.Errorf("KeysPushed = %v", results[0].KeysPushed)
	}
}

func TestRunHermesSync_DryRunSkipsDial(t *testing.T) {
	resetHermesFlags()
	defer resetHermesFlags()

	_, cleanup := withHermesTestDir(t,
		"OPENROUTER_API_KEY=sk-or-test\n",
		"model: nvidia/nemotron-3-super-120b-a12b\n")
	defer cleanup()
	cleanup2 := withHermesTestNodes(t, "hermes@claw1.local\n")
	defer cleanup2()

	hermesAll = true
	hermesDryRun = true

	dialCalled := false
	prev := newDialer
	newDialer = func(_ ...osh.Option) Dialer {
		dialCalled = true
		return &fakeHermesDialer{}
	}
	defer func() { newDialer = prev }()

	var buf bytes.Buffer
	hermesSyncCmd.SetOut(&buf)
	defer hermesSyncCmd.SetOut(nil)

	if err := runHermesSync(hermesSyncCmd, nil); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if dialCalled {
		t.Errorf("dry-run should not dial workers")
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Errorf("dry-run output missing marker: %q", buf.String())
	}
}

func TestRunHermesSync_NoAuthKeys(t *testing.T) {
	resetHermesFlags()
	defer resetHermesFlags()
	_, cleanup := withHermesTestDir(t, "# no auth keys here\nTERMINAL_TIMEOUT=30\n", "")
	defer cleanup()
	cleanup2 := withHermesTestNodes(t, "hermes@claw1.local\n")
	defer cleanup2()
	hermesAll = true
	defer withHermesTestLog(t)()

	err := runHermesSync(hermesSyncCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no recognized auth keys") {
		t.Errorf("expected 'no recognized auth keys', got %v", err)
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func equalStrSlices(a, b []string) bool {
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

// satisfy "imported and not used" for io if no other use materializes
var _ = io.EOF
var _ time.Duration
