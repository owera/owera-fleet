package hermes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner records every exec.Cmd the package would have launched and
// fabricates a deterministic outcome for it. It is the heart of every
// test in this file: by snapshotting the argv, env, and stdin of each
// invocation we can assert the secrets channel invariant.
type fakeRunner struct {
	calls []recordedCall

	// nextStdout / nextStderr / nextExit are consumed in order; if the
	// test queues fewer responses than calls, the trailing calls get
	// the default (empty stdout, exit 0).
	nextStdout []string
	nextStderr []string
	nextExit   []int

	// returnErr, when non-empty for a given call index, makes runCmd
	// return that error instead of synthesizing an ExitError. Used to
	// test ctx-cancel paths.
	returnErr []error
}

type recordedCall struct {
	binary string
	args   []string
	env    []string // nil means inherited; we record cmd.Env verbatim
	stdin  string   // captured by draining cmd.Stdin if set
}

func (f *fakeRunner) runCmd(ctx context.Context, cmd *exec.Cmd) error {
	rc := recordedCall{
		binary: cmd.Path,
		args:   append([]string(nil), cmd.Args[1:]...), // strip argv[0]
		env:    append([]string(nil), cmd.Env...),
	}
	if cmd.Stdin != nil {
		b, _ := io.ReadAll(cmd.Stdin)
		rc.stdin = string(b)
	}
	i := len(f.calls)
	f.calls = append(f.calls, rc)

	if i < len(f.nextStdout) && cmd.Stdout != nil {
		_, _ = io.WriteString(cmd.Stdout, f.nextStdout[i])
	}
	if i < len(f.nextStderr) && cmd.Stderr != nil {
		_, _ = io.WriteString(cmd.Stderr, f.nextStderr[i])
	}
	if i < len(f.returnErr) && f.returnErr[i] != nil {
		return f.returnErr[i]
	}
	if i < len(f.nextExit) && f.nextExit[i] != 0 {
		// Use /usr/bin/false / true via a small trick: we can't fabricate
		// an *exec.ExitError directly (its fields are unexported and OS
		// specific), so simulate it by actually running a stub command
		// that exits with the requested code.
		stub := exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("exit %d", f.nextExit[i]))
		return stub.Run()
	}
	return nil
}

// newTestClient wires a Client against the supplied fake plus a fake
// readFile returning pinned, and a lookPath returning a synthetic binary
// path. Helper to keep individual tests short.
func newTestClient(t *testing.T, runner *fakeRunner, pinned string) *Client {
	t.Helper()
	lookPath := func(name string) (string, error) {
		if name != "hermes" {
			return "", fmt.Errorf("unexpected lookPath: %s", name)
		}
		return "/fake/bin/hermes", nil
	}
	readFile := func(path string) ([]byte, error) {
		if !strings.HasSuffix(path, "PINNED_VERSION") {
			return nil, fmt.Errorf("unexpected readFile: %s", path)
		}
		return []byte(pinned + "\n"), nil
	}
	c, err := NewWithDeps(lookPath, readFile, runner.runCmd, "/tmp/.hermes-fake")
	if err != nil {
		t.Fatalf("NewWithDeps: %v", err)
	}
	return c
}

func TestNewWithDepsResolvesBinaryAndPin(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(t, r, "v0.13.0")
	if c.Binary() != "/fake/bin/hermes" {
		t.Errorf("Binary = %q, want /fake/bin/hermes", c.Binary())
	}
	if c.Pinned() != "v0.13.0" {
		t.Errorf("Pinned = %q, want v0.13.0", c.Pinned())
	}
}

func TestNewWithDepsBinaryNotFound(t *testing.T) {
	lookPath := func(string) (string, error) { return "", exec.ErrNotFound }
	readFile := func(string) ([]byte, error) { return []byte("v0.13.0"), nil }
	runCmd := func(context.Context, *exec.Cmd) error { return nil }
	_, err := NewWithDeps(lookPath, readFile, runCmd, "/tmp/.hermes-fake")
	if !errors.Is(err, ErrBinaryNotFound) {
		t.Fatalf("err = %v, want ErrBinaryNotFound", err)
	}
}

func TestNewWithDepsEmptyPin(t *testing.T) {
	lookPath := func(string) (string, error) { return "/fake/bin/hermes", nil }
	readFile := func(string) ([]byte, error) { return []byte("\n   \n"), nil }
	runCmd := func(context.Context, *exec.Cmd) error { return nil }
	_, err := NewWithDeps(lookPath, readFile, runCmd, "/tmp/.hermes-fake")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want empty-pin error", err)
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"canonical-v0.13.0-with-build", "Hermes Agent v0.13.0 (2026.5.7)\nProject: ...\nPython: 3.14.5", "v0.13.0"},
		{"lowercase-hermes-no-v", "hermes 0.13.0", "v0.13.0"},
		{"hermes-agent-rc", "hermes-agent v0.14.0-rc1", "v0.14.0-rc1"},
		{"capital-with-leading-blank", "\n\nHermes Agent v1.0.0", "v1.0.0"},
		{"unrelated-text", "this is not a hermes version line", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseVersion(tc.in)
			if got != tc.want {
				t.Errorf("ParseVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestVersionFromCLI(t *testing.T) {
	r := &fakeRunner{
		nextStdout: []string{"Hermes Agent v0.13.0 (2026.5.7)\n"},
	}
	c := newTestClient(t, r, "v0.13.0")
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "v0.13.0" {
		t.Errorf("Version = %q, want v0.13.0", v)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	if got := r.calls[0].args; len(got) != 1 || got[0] != "--version" {
		t.Errorf("args = %v, want [--version]", got)
	}
}

func TestVersionUnparseable(t *testing.T) {
	r := &fakeRunner{
		nextStdout: []string{"some other tool"},
		nextStderr: []string{""},
	}
	c := newTestClient(t, r, "v0.13.0")
	_, err := c.Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not parse version") {
		t.Fatalf("err = %v, want parse-failure", err)
	}
}

func TestVerifyPinnedMatch(t *testing.T) {
	r := &fakeRunner{
		nextStdout: []string{"Hermes Agent v0.13.0 (2026.5.7)\n"},
	}
	c := newTestClient(t, r, "v0.13.0")
	if err := c.VerifyPinned(context.Background()); err != nil {
		t.Fatalf("VerifyPinned: %v", err)
	}
}

func TestVerifyPinnedMatchTolerantOfMissingV(t *testing.T) {
	// Pin file without the leading 'v' should still match a CLI that
	// prints one (and vice versa).
	r := &fakeRunner{
		nextStdout: []string{"hermes 0.13.0\n"},
	}
	c := newTestClient(t, r, "v0.13.0")
	if err := c.VerifyPinned(context.Background()); err != nil {
		t.Fatalf("VerifyPinned: %v", err)
	}
}

func TestVerifyPinnedMismatch(t *testing.T) {
	r := &fakeRunner{
		nextStdout: []string{"Hermes Agent v0.14.0 (2026.6.1)\n"},
	}
	c := newTestClient(t, r, "v0.13.0")
	err := c.VerifyPinned(context.Background())
	if err == nil {
		t.Fatal("VerifyPinned: want error, got nil")
	}
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("err = %v, want errors.Is ErrVersionMismatch", err)
	}
	if !strings.Contains(err.Error(), "live=v0.14.0") || !strings.Contains(err.Error(), "pinned=v0.13.0") {
		t.Errorf("err should name both versions, got: %v", err)
	}
}

func TestRunZBuildsArgvAndCapturesOutput(t *testing.T) {
	r := &fakeRunner{
		nextStdout: []string{"## Findings\n..."},
		nextStderr: []string{""},
	}
	c := newTestClient(t, r, "v0.13.0")
	res, err := c.RunZ(context.Background(), "research X",
		WithToolset("web,browser"),
		WithModel("nvidia/nemotron-3"),
		WithMaxTurns(8),
	)
	if err != nil {
		t.Fatalf("RunZ: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "## Findings") {
		t.Errorf("Stdout missing findings: %q", res.Stdout)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d", len(r.calls))
	}
	args := r.calls[0].args
	want := []string{"-z", "research X", "-t", "web,browser", "-m", "nvidia/nemotron-3", "--max-turns", "8", "--accept-hooks"}
	if !stringsEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestRunZPropagatesExitCode(t *testing.T) {
	r := &fakeRunner{
		nextStdout: []string{""},
		nextStderr: []string{"boom\n"},
		nextExit:   []int{17},
	}
	c := newTestClient(t, r, "v0.13.0")
	res, err := c.RunZ(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("RunZ: %v", err)
	}
	if res.ExitCode != 17 {
		t.Errorf("ExitCode = %d, want 17", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Errorf("Stderr = %q, want to contain 'boom'", res.Stderr)
	}
}

func TestRunZStderrTailTruncation(t *testing.T) {
	bigStderr := strings.Repeat("X", MaxStderrTailBytes+1000) + "TAIL"
	r := &fakeRunner{
		nextStdout: []string{""},
		nextStderr: []string{bigStderr},
	}
	c := newTestClient(t, r, "v0.13.0")
	res, err := c.RunZ(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("RunZ: %v", err)
	}
	if len(res.Stderr) != MaxStderrTailBytes {
		t.Errorf("len(Stderr) = %d, want %d", len(res.Stderr), MaxStderrTailBytes)
	}
	if !strings.HasSuffix(res.Stderr, "TAIL") {
		t.Errorf("Stderr should keep tail, got suffix: %q", res.Stderr[len(res.Stderr)-10:])
	}
}

// TestRunWithSecretsNoLeak is the load-bearing invariant test. The
// CLAUDE.md guidance is explicit: secrets MUST NOT appear in argv or
// env. This test loads up several "secrets" (with values designed to be
// trivially grep-able), invokes RunWithSecrets, and then walks the
// captured argv + env to confirm not a single secret value leaked into
// either channel.
func TestRunWithSecretsNoLeak(t *testing.T) {
	r := &fakeRunner{
		nextStdout: []string{"ok"},
	}
	c := newTestClient(t, r, "v0.13.0")
	secrets := map[string]string{
		"OPENROUTER_API_KEY": "sk-secret-canary-value-001",
		"BRAVE_API_KEY":      "br-secret-canary-value-002",
		"STRIPE_KEY":         "sk_live_canary_003",
	}
	_, err := c.RunWithSecrets(context.Background(), "do thing", secrets,
		WithToolset("web"),
	)
	if err != nil {
		t.Fatalf("RunWithSecrets: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d", len(r.calls))
	}
	call := r.calls[0]

	// --- THE LOAD-BEARING ASSERTION ---
	// Build a single haystack from argv + env and grep for each secret
	// value. The test must fail loudly if any secret material reaches
	// either observable channel.
	haystack := strings.Join(call.args, "\x00") + "\x00" + strings.Join(call.env, "\x00")
	for k, v := range secrets {
		if strings.Contains(haystack, v) {
			t.Errorf("secret value for %s LEAKED into argv/env: hay=%q", k, haystack)
		}
		// The KEY *may* legitimately appear in the prompt or argv, so
		// we only assert on the value. But we still confirm the env was
		// not seeded with KEY=value, which is the most likely leak path.
		for _, e := range call.env {
			if strings.HasPrefix(e, k+"=") {
				t.Errorf("env contains KEY=value for %s: %q", k, e)
			}
		}
	}

	// Confirm the secrets DID arrive via stdin in the canonical wire
	// format — otherwise the test would be vacuously true (a no-op
	// implementation would also pass the leak check).
	for k, v := range secrets {
		line := k + "=" + v + "\n"
		if !strings.Contains(call.stdin, line) {
			t.Errorf("stdin missing %q line; got: %q", line, call.stdin)
		}
	}

	// Belt and braces: cmd.Env must be nil (inherited) so the OS-level
	// parent environment is the only env the child sees. Anything else
	// risks accidental leaks during refactors.
	if len(call.env) != 0 {
		t.Errorf("cmd.Env = %v, want nil/empty (inherited)", call.env)
	}
}

func TestRunWithSecretsDeterministicOrder(t *testing.T) {
	r := &fakeRunner{nextStdout: []string{""}}
	c := newTestClient(t, r, "v0.13.0")
	secrets := map[string]string{
		"ZZZ_KEY": "z",
		"AAA_KEY": "a",
		"MMM_KEY": "m",
	}
	if _, err := c.RunWithSecrets(context.Background(), "p", secrets); err != nil {
		t.Fatalf("RunWithSecrets: %v", err)
	}
	want := "AAA_KEY=a\nMMM_KEY=m\nZZZ_KEY=z\n"
	if r.calls[0].stdin != want {
		t.Errorf("stdin = %q, want %q", r.calls[0].stdin, want)
	}
}

func TestRunWithSecretsRejectsEmptyMap(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(t, r, "v0.13.0")
	_, err := c.RunWithSecrets(context.Background(), "p", nil)
	if err == nil || !strings.Contains(err.Error(), "at least one secret") {
		t.Fatalf("err = %v, want at-least-one error", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("calls = %d, want 0 (must fail before spawning)", len(r.calls))
	}
}

func TestRunWithSecretsRejectsBadKey(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(t, r, "v0.13.0")
	_, err := c.RunWithSecrets(context.Background(), "p", map[string]string{"BAD KEY": "v"})
	if err == nil || !strings.Contains(err.Error(), "forbidden char") {
		t.Fatalf("err = %v, want forbidden-char error", err)
	}
}

func TestRunWithSecretsRejectsNewlineValue(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(t, r, "v0.13.0")
	_, err := c.RunWithSecrets(context.Background(), "p", map[string]string{"K": "line1\nline2"})
	if err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("err = %v, want newline error", err)
	}
}

func TestRunZWithStdin(t *testing.T) {
	r := &fakeRunner{nextStdout: []string{""}}
	c := newTestClient(t, r, "v0.13.0")
	body := "extra prompt content piped in"
	_, err := c.RunZ(context.Background(), "prompt", WithStdin(strings.NewReader(body)))
	if err != nil {
		t.Fatalf("RunZ: %v", err)
	}
	if r.calls[0].stdin != body {
		t.Errorf("stdin = %q, want %q", r.calls[0].stdin, body)
	}
}

func TestRunZTimeoutFires(t *testing.T) {
	r := &fakeRunner{
		returnErr: []error{context.DeadlineExceeded},
	}
	c := newTestClient(t, r, "v0.13.0")
	_, err := c.RunZ(context.Background(), "prompt", WithTimeout(10*time.Millisecond))
	if err == nil {
		t.Fatal("RunZ: want error from timeout, got nil")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
		t.Errorf("err = %v, want ctx-related error", err)
	}
}

func TestResolvePinPathOverride(t *testing.T) {
	got, err := resolvePinPath("/custom/dir")
	if err != nil {
		t.Fatalf("resolvePinPath: %v", err)
	}
	want := filepath.Join("/custom/dir", "PINNED_VERSION")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func stringsEqual(a, b []string) bool {
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
