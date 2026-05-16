// Package hermes is the thin typed wrapper around the local `hermes` CLI
// binary (Nous Research's Hermes Agent, v0.13.0 as of 2026-05-15).
//
// Responsibilities:
//
//  1. Discover the hermes binary on PATH.
//  2. Enforce a pinned version: read ~/.hermes/PINNED_VERSION and compare
//     against `hermes --version` so an operator can't accidentally drift
//     onto a release this fleet has not been validated against.
//  3. Invoke `hermes -z PROMPT ...` for one-shot research / review / triage
//     work and capture stdout, stderr, exit code, and duration.
//
// # Secrets handling
//
// The CLI exposes secrets in two operationally-safe channels: stdin and
// well-known environment-variable names the operator has already set in
// the gateway's launchd plist or shell rc. This package only ever passes
// secrets via stdin, as KEY=value\n lines, through [Client.RunWithSecrets].
//
// Concretely: secrets MUST NOT appear in argv (visible to anyone with
// `ps`), MUST NOT be exported into the child's env (visible in /proc/PID
// equivalents and in crash dumps), and MUST NOT be written to disk by
// this package. That invariant is enforced by tests; see TestRunWithSecrets.
//
// # Dependency injection
//
// Production callers use [Default], which wires production implementations
// of the three side-effecting functions (look up the binary in PATH, read
// files from disk, exec subprocesses). Tests construct a [Client] directly
// with fakes — see hermes_test.go for the pattern.
package hermes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultPinnedVersionPath is the relative path under $HOME where the
// version pin lives. The hermes-setup prototype seeded the same file, and
// internal/state reads it for the snapshot — we resolve it identically so
// the two surfaces never disagree.
const DefaultPinnedVersionPath = ".hermes/PINNED_VERSION"

// MaxStderrTailBytes caps the stderr capture a Result carries. Matches
// internal/log.MaxStderrTailBytes (which is 2 KiB), but doubled to 4 KiB
// here because hermes traces are chatty and the first failure signal is
// often near the top of a multi-step turn. Callers that forward into the
// log package will get a second truncation there.
const MaxStderrTailBytes = 4096

// ErrVersionMismatch is returned from [Client.VerifyPinned] when the live
// `hermes --version` output disagrees with the contents of the pin file.
// Sentinel so callers can errors.Is against it (the future
// `fleetctl version-check` command relies on this).
var ErrVersionMismatch = errors.New("hermes: version mismatch")

// ErrBinaryNotFound is returned from [Default] when the hermes binary is
// not on PATH. Surfaced separately so the operator gets a precise hint
// instead of a generic exec.ErrNotFound wrap.
var ErrBinaryNotFound = errors.New("hermes: binary not on PATH")

// Result is the structured outcome of one CLI invocation. Mirrors the
// shape internal/log uses for the `stderr_tail` and `duration_ms` fields
// so callers can fold it into a [log.Event] without reshaping.
type Result struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	DurationMs int64
}

// Options carry the optional knobs accepted by [Client.RunZ] and
// [Client.RunWithSecrets]. Construct via the With* helpers; the zero
// value is a valid "no options" set.
type Options struct {
	toolset string
	model   string
	timeout time.Duration
	maxTurns int
	stdin   io.Reader
	extraArgs []string
}

// Option is a functional setter for [Options]. Use the With* helpers
// rather than constructing one by hand — the field is unexported.
type Option func(*Options)

// WithToolset enables a comma-separated set of Hermes toolsets for the
// invocation (e.g. "web,browser", "fs", "shell"). Maps to `-t TOOLSET`.
func WithToolset(name string) Option {
	return func(o *Options) { o.toolset = name }
}

// WithModel overrides the OpenRouter model the agent uses for the turn.
// Maps to `-m MODEL`. Empty means "use the hermes default" (which itself
// is read from ~/.hermes/config.yaml).
func WithModel(name string) Option {
	return func(o *Options) { o.model = name }
}

// WithTimeout caps the total subprocess runtime. Implemented via the
// context handed to exec.CommandContext — when it fires, the subprocess
// is killed and Run* returns ctx.Err() wrapped with the duration so far.
// Zero (the default) means "no cap; rely on the parent ctx".
func WithTimeout(d time.Duration) Option {
	return func(o *Options) { o.timeout = d }
}

// WithMaxTurns caps the number of agent reasoning turns. Maps to
// `--max-turns N`. Zero means "let hermes use its config default".
func WithMaxTurns(n int) Option {
	return func(o *Options) { o.maxTurns = n }
}

// WithStdin pipes r into the subprocess's stdin. RunWithSecrets composes
// over this internally; direct callers can use it for prompt files or
// other non-secret data. If both WithStdin and RunWithSecrets-supplied
// secrets are present, the secrets win (and the original reader is
// ignored to keep the channel single-source).
func WithStdin(r io.Reader) Option {
	return func(o *Options) { o.stdin = r }
}

// Client wraps the local hermes binary plus the pin state read from disk.
// Construct via [Default] for production; tests construct it directly
// with fakes via [NewWithDeps].
type Client struct {
	binary        string
	pinnedVersion string
	deps          deps
}

// deps is the dependency-injected surface. All three fields are required
// — leaving one nil will panic on use. Kept unexported because the only
// supported way to override is through [NewWithDeps] in tests.
type deps struct {
	lookPath func(name string) (string, error)
	readFile func(path string) ([]byte, error)
	runCmd   func(ctx context.Context, cmd *exec.Cmd) error
}

// productionDeps are the deps wired by [Default]: PATH lookup via
// exec.LookPath, file reads via os.ReadFile, and subprocess execution
// via cmd.Run (which honours the context the caller already plumbed in
// via exec.CommandContext).
func productionDeps() deps {
	return deps{
		lookPath: exec.LookPath,
		readFile: os.ReadFile,
		runCmd:   func(_ context.Context, cmd *exec.Cmd) error { return cmd.Run() },
	}
}

// Default discovers `hermes` on PATH, reads ~/.hermes/PINNED_VERSION,
// and returns a production-wired [Client]. Either failure (no binary, no
// pin file) is reported with a wrapped error that names the missing
// artifact so the operator can act without digging.
func Default() (*Client, error) {
	d := productionDeps()
	return newClient(d, "")
}

// NewWithDeps is the test seam: callers supply their own lookPath,
// readFile, and runCmd functions, plus an optional hermesDir override
// (empty falls back to $HOME/.hermes). Panics if any dependency is nil
// to fail loudly on a wiring mistake.
func NewWithDeps(
	lookPath func(string) (string, error),
	readFile func(string) ([]byte, error),
	runCmd func(context.Context, *exec.Cmd) error,
	hermesDir string,
) (*Client, error) {
	if lookPath == nil || readFile == nil || runCmd == nil {
		panic("hermes: NewWithDeps requires all three deps")
	}
	return newClient(deps{lookPath: lookPath, readFile: readFile, runCmd: runCmd}, hermesDir)
}

func newClient(d deps, hermesDir string) (*Client, error) {
	binPath, err := d.lookPath("hermes")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBinaryNotFound, err)
	}
	pinPath, err := resolvePinPath(hermesDir)
	if err != nil {
		return nil, err
	}
	raw, err := d.readFile(pinPath)
	if err != nil {
		return nil, fmt.Errorf("hermes: read pin %s: %w", pinPath, err)
	}
	pinned := strings.TrimSpace(string(raw))
	if pinned == "" {
		return nil, fmt.Errorf("hermes: pin file %s is empty", pinPath)
	}
	return &Client{binary: binPath, pinnedVersion: pinned, deps: d}, nil
}

func resolvePinPath(hermesDir string) (string, error) {
	if hermesDir != "" {
		return filepath.Join(hermesDir, "PINNED_VERSION"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("hermes: $HOME unset; cannot locate PINNED_VERSION")
	}
	return filepath.Join(home, DefaultPinnedVersionPath), nil
}

// Binary returns the resolved absolute path to the hermes CLI.
func (c *Client) Binary() string { return c.binary }

// Pinned returns the version recorded in PINNED_VERSION (already trimmed,
// canonical, e.g. "v0.13.0"). Useful for callers that want to display the
// pin without re-reading the file.
func (c *Client) Pinned() string { return c.pinnedVersion }

// versionLineRE matches the canonical first line of `hermes --version`:
//
//	Hermes Agent v0.13.0 (2026.5.7)
//	hermes 0.13.0
//	hermes-agent v0.13.0-rc1
//
// Captures the leading-v-or-not semver token in group 1. The trailing
// build metadata in parens is ignored on purpose: pin comparison is
// against the human-typed version, not the wheel build date.
var versionLineRE = regexp.MustCompile(`(?i)(?:hermes(?:[- ]agent)?)\s+(v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?)`)

// ParseVersion extracts the canonical version token from one line of
// `hermes --version` output. Returns "" if no recognizable token is
// present so the caller can surface a precise "could not parse" error.
//
// Canonical form is "vMAJOR.MINOR.PATCH[-prerelease]" — a missing 'v'
// prefix is added so the rest of the package can compare strings.
func ParseVersion(out string) string {
	for _, raw := range strings.Split(out, "\n") {
		m := versionLineRE.FindStringSubmatch(raw)
		if len(m) >= 2 {
			v := m[1]
			if !strings.HasPrefix(v, "v") {
				v = "v" + v
			}
			return v
		}
	}
	return ""
}

// Version runs `hermes --version` and returns the canonical version
// string. The subprocess is bounded by ctx; if ctx is already cancelled
// when Version is called it returns ctx.Err().
func (c *Client) Version(ctx context.Context) (string, error) {
	r, err := c.run(ctx, []string{"--version"}, Options{}, nil)
	if err != nil {
		return "", fmt.Errorf("hermes --version: %w", err)
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("hermes --version exited %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	v := ParseVersion(r.Stdout)
	if v == "" {
		// Fall back to scanning stderr — some CLIs print --version there.
		v = ParseVersion(r.Stderr)
	}
	if v == "" {
		return "", fmt.Errorf("hermes: could not parse version from %q", strings.TrimSpace(r.Stdout))
	}
	return v, nil
}

// VerifyPinned reads `hermes --version` and compares it against the pin.
// Returns [ErrVersionMismatch] (wrapped, with both versions visible) on
// disagreement and nil on match. Any I/O failure is returned unwrapped.
func (c *Client) VerifyPinned(ctx context.Context) error {
	live, err := c.Version(ctx)
	if err != nil {
		return err
	}
	if !versionsEqual(live, c.pinnedVersion) {
		return fmt.Errorf("%w: live=%s pinned=%s", ErrVersionMismatch, live, c.pinnedVersion)
	}
	return nil
}

// versionsEqual tolerates an optional leading 'v' on either side so a
// pin file written as "0.13.0" matches a CLI that prints "v0.13.0".
func versionsEqual(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// RunZ invokes `hermes -z PROMPT` with the supplied options and returns
// the captured Result. Stderr is truncated tail-first to
// [MaxStderrTailBytes] to keep the Result bounded.
//
// The prompt is passed as an argv element. Callers that need to pass
// *secret* material with the prompt (API keys for tools, etc.) must use
// [Client.RunWithSecrets] instead — argv is observable.
func (c *Client) RunZ(ctx context.Context, prompt string, opts ...Option) (Result, error) {
	o := buildOptions(opts)
	args := append([]string{"-z", prompt}, oneShotFlags(o)...)
	return c.run(ctx, args, o, nil)
}

// RunWithSecrets invokes the CLI like [RunZ] but pipes secrets in as
// KEY=value\n lines on stdin. The CLI is expected to read them with a
// dedicated --secrets-stdin-style flag (T8.3 lands the operator-facing
// command that uses it). Until T8.3 wires the actual flag name, this
// package piping is the building block — the channel guarantee (never
// argv/env/disk) holds regardless of the consuming flag.
//
// secrets MUST NOT be referenced from anywhere else: not the prompt
// (since that's argv), not WithStdin (because that's a non-secret data
// channel), not options. The caller is responsible for ensuring the
// prompt only references secrets by their KEY name and lets the agent
// resolve via the stdin map.
func (c *Client) RunWithSecrets(ctx context.Context, prompt string, secrets map[string]string, opts ...Option) (Result, error) {
	o := buildOptions(opts)
	// Build the stdin payload first so an empty/invalid map fails before
	// any subprocess is spawned.
	payload, err := encodeSecrets(secrets)
	if err != nil {
		return Result{}, err
	}
	o.stdin = bytes.NewReader(payload)
	args := append([]string{"-z", prompt}, oneShotFlags(o)...)
	return c.run(ctx, args, o, payload)
}

// encodeSecrets serializes secrets as KEY=value\n lines in deterministic
// key order. Rejects empty keys, keys containing '=' or whitespace, and
// values containing newlines (which would break the line-oriented
// framing). The exact wire format is the inter-process contract with the
// hermes CLI's secrets-stdin reader.
func encodeSecrets(secrets map[string]string) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, errors.New("hermes: RunWithSecrets requires at least one secret")
	}
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		if k == "" {
			return nil, errors.New("hermes: empty secret key")
		}
		if strings.ContainsAny(k, "=\n\t \r") {
			return nil, fmt.Errorf("hermes: secret key %q contains forbidden char", k)
		}
		keys = append(keys, k)
	}
	// Sort for deterministic ordering — makes tests stable and means
	// strace output is reproducible across runs.
	sortStrings(keys)
	var buf bytes.Buffer
	for _, k := range keys {
		v := secrets[k]
		if strings.ContainsAny(v, "\n\r") {
			return nil, fmt.Errorf("hermes: secret value for %q contains newline", k)
		}
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(v)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// sortStrings is a 5-line insertion sort to avoid pulling sort just for
// the secret-key ordering. encodeSecrets is hot only when the caller is
// running a hermes turn (multi-second), so allocation profile doesn't
// matter; the smaller dep graph for this package does.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// oneShotFlags converts an [Options] into the argv tail accepted by
// `hermes -z`. Returns a fresh slice each call so the caller can append.
func oneShotFlags(o Options) []string {
	var args []string
	if o.toolset != "" {
		args = append(args, "-t", o.toolset)
	}
	if o.model != "" {
		args = append(args, "-m", o.model)
	}
	if o.maxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", o.maxTurns))
	}
	// --accept-hooks: matches the bash prototype's invocation; required
	// to bypass interactive hook approval when running non-interactively.
	args = append(args, "--accept-hooks")
	args = append(args, o.extraArgs...)
	return args
}

func buildOptions(opts []Option) Options {
	var o Options
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// run is the single subprocess entry point. All flag assembly happens in
// the callers; run only handles ctx/timeout, stdin wiring, output
// capture, and stderr truncation.
//
// secretsPayload, when non-nil, is the canonical secrets blob — passed
// through so run can wire it into stdin without trusting the optional
// WithStdin field. When it's nil, run honors o.stdin (which may also be
// nil for "no stdin").
func (c *Client) run(ctx context.Context, args []string, o Options, secretsPayload []byte) (Result, error) {
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, c.binary, args...)
	// CRITICAL invariant for the secrets channel: never set cmd.Env to
	// anything derived from secret material. We deliberately inherit the
	// parent's env (cmd.Env = nil leaves it inherited) so things like
	// OPENROUTER_API_KEY that the operator pre-loaded into the launchd
	// plist still flow, but no secret passed via RunWithSecrets ever
	// touches Env. The test TestRunWithSecretsNoLeak pins this.
	cmd.Env = nil

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	switch {
	case secretsPayload != nil:
		cmd.Stdin = bytes.NewReader(secretsPayload)
	case o.stdin != nil:
		cmd.Stdin = o.stdin
	}

	start := time.Now()
	runErr := c.deps.runCmd(ctx, cmd)
	dur := time.Since(start).Milliseconds()

	r := Result{
		Stdout:     stdoutBuf.String(),
		Stderr:     truncateTail(stderrBuf.String(), MaxStderrTailBytes),
		DurationMs: dur,
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			r.ExitCode = exitErr.ExitCode()
			return r, nil
		}
		// Distinguish ctx-driven timeout / cancel from a generic exec
		// failure so callers can surface "the agent ran past the cap".
		if ctxErr := ctx.Err(); ctxErr != nil {
			return r, fmt.Errorf("hermes: %w (after %dms)", ctxErr, dur)
		}
		return r, fmt.Errorf("hermes: exec: %w", runErr)
	}
	r.ExitCode = 0
	return r, nil
}

// truncateTail keeps the last max bytes of s. We prefer the tail because
// agent traces print the immediate failure at the bottom; the prompt
// echo at the top is rarely useful at fail time.
func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
