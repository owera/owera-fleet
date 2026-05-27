// hermes.go implements `fleetctl hermes` — read and reconcile per-node
// hermes-agent configuration (LLM auth keys + model selection) using the
// gateway's ~/.hermes/.env and config.yaml as source of truth.
//
// Three subcommands:
//
//	fleetctl hermes audit         read-only per-node `hermes status` summary
//	fleetctl hermes sync          push gateway auth keys (+ model) to workers
//	fleetctl hermes ensure-config audit + sync any nodes that need it
//
// Credentials are streamed via SSH stdin (never embedded in command lines)
// so they never appear in remote process listings or our JSONL audit log.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

// HERMES_AUTH_KEYS is the closed-set list of env-var names we will read
// from the gateway's plain ~/.hermes/.env and distribute to workers.
// Anything outside this list is treated as worker-local config and left
// alone — workers commonly have toolset knobs (BROWSER_*, TERMINAL_*)
// that must not be overwritten.
var hermesAuthKeys = []string{
	"OPENROUTER_API_KEY",
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_TOKEN",
	"GOOGLE_API_KEY",
	"GEMINI_API_KEY",
	"DEEPSEEK_API_KEY",
	"XAI_API_KEY",
	"NVIDIA_API_KEY",
	"GROQ_API_KEY",
	"TOGETHER_API_KEY",
	"MISTRAL_API_KEY",
}

var (
	hermesNode       string
	hermesAll        bool
	hermesJSON       bool
	hermesQuiet      bool
	hermesTimeout    time.Duration
	hermesHermesDir  string
	hermesNodesPath  string
	hermesNoModel    bool
	hermesDryRun     bool
	hermesLogPath    = "~/.hermes/logs/hermes.jsonl"
)

// HermesStatus is the parsed, structured form of `hermes status`.
type HermesStatus struct {
	Target          string            `json:"target"`
	ExitCode        int               `json:"exit_code"`
	DurationMs      int64             `json:"duration_ms"`
	Configured      bool              `json:"configured"`
	NeedsSetup      bool              `json:"needs_setup"`
	Model           string            `json:"model,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	KeysPresent     []string          `json:"keys_present"`
	ProviderHasKey  bool              `json:"provider_has_key"`
	HermesAvailable bool              `json:"hermes_available"`
	Raw             string            `json:"raw,omitempty"`
	Error           string            `json:"error,omitempty"`
	Extra           map[string]string `json:"-"`
}

// SyncResult is the per-target outcome of a sync operation.
type SyncResult struct {
	Target        string `json:"target"`
	ExitCode      int    `json:"exit_code"`
	DurationMs    int64  `json:"duration_ms"`
	KeysPushed    []string `json:"keys_pushed"`
	ModelApplied  string `json:"model_applied,omitempty"`
	BeforeStatus  string `json:"-"`
	AfterStatus   string `json:"after_status,omitempty"`
	Configured    bool   `json:"configured"`
	Error         string `json:"error,omitempty"`
}

// hermesCmd is the parent; the three RunE commands are children.
var hermesCmd = &cobra.Command{
	Use:   "hermes",
	Short: "Per-node hermes config audit + sync (LLM keys, model)",
	Long: `Manage per-node hermes-agent configuration across the fleet.

Workers are bootstrapped with the structural config (config.yaml with paths
rewritten) but never receive provider API keys — those live only in the
gateway's plain ~/.hermes/.env. The 'sync' and 'ensure-config' subcommands
distribute the gateway's auth keys (OPENROUTER_API_KEY, ANTHROPIC_*, …) to
selected workers via SSH stdin so credentials never appear in command lines.

By default 'sync' also mirrors the gateway's 'model:' setting into each
worker's config.yaml so the fleet stays homogeneous; pass --no-model to skip.`,
}

var hermesAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Per-node `hermes status` summary (read-only)",
	Long: `Runs 'hermes status' on each selected node and reports configured/needs-setup
plus the detected Model, Provider, and API-key presence. Read-only, no writes.`,
	Example: `  fleetctl hermes audit
  fleetctl hermes audit --json
  fleetctl hermes audit --node hermes@claw1.local`,
	RunE: runHermesAudit,
}

var hermesSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Push gateway hermes auth keys (+ model) to selected workers",
	Long: `Reads the gateway's ~/.hermes/.env, extracts the allow-listed auth-key
lines (OPENROUTER_API_KEY, ANTHROPIC_*, etc.), and merges them into each
selected worker's ~/.hermes/.env. Worker's other env values are preserved.

Also (unless --no-model) reads the gateway's current 'model:' value from
~/.hermes/config.yaml and writes the same value into each worker's
config.yaml. The fleet ends up homogeneous on provider+model.

Credential payload is sent over SSH stdin — never on the command line.
Subprocess argv and remote process listings show only generic 'cat > …'
and 'sed -i' commands; secret values are seen only by the destination
process.

Targets resolve like 'fleetctl delegate' (--node / --all / --random); the
default is --all.`,
	Example: `  fleetctl hermes sync --all
  fleetctl hermes sync --node hermes@claw1.local
  fleetctl hermes sync --all --no-model
  fleetctl hermes sync --all --dry-run`,
	RunE: runHermesSync,
}

var hermesEnsureCmd = &cobra.Command{
	Use:   "ensure-config",
	Short: "Audit + sync any nodes whose hermes config is incomplete",
	Long: `Idempotent. Runs 'audit' first; for any node that reports needs_setup
(missing API key for its configured provider, or hermes wholly unconfigured),
runs 'sync' against that subset. Already-configured nodes are not touched.

Designed to be safe to run repeatedly — e.g. from a cron, from a hook on
'fleetctl bootstrap-worker', or as a fleet-wide reconcile after rotating
the gateway's API keys.`,
	Example: `  fleetctl hermes ensure-config
  fleetctl hermes ensure-config --json
  fleetctl hermes ensure-config --node hermes@claw1.local`,
	RunE: runHermesEnsure,
}

func init() {
	// Shared flags across all three subcommands.
	for _, c := range []*cobra.Command{hermesAuditCmd, hermesSyncCmd, hermesEnsureCmd} {
		c.Flags().StringVar(&hermesNode, "node", "", "target one node (user@host); mutually exclusive with --all")
		c.Flags().BoolVar(&hermesAll, "all", false, "target every entry in ~/.hermes/nodes.txt (default for ensure-config and sync)")
		c.Flags().BoolVar(&hermesJSON, "json", false, "emit JSON array instead of markdown/text")
		c.Flags().BoolVar(&hermesQuiet, "quiet", false, "suppress stdout; JSONL log still written")
		c.Flags().DurationVar(&hermesTimeout, "timeout", 30*time.Second, "per-node SSH connect+run cap")
		c.Flags().StringVar(&hermesHermesDir, "hermes-dir", "~/.hermes", "override ~/.hermes path on the gateway")
		c.Flags().StringVar(&hermesNodesPath, "nodes-file", "", "override ~/.hermes/nodes.txt path")
	}
	hermesSyncCmd.Flags().BoolVar(&hermesNoModel, "no-model", false, "skip model line; push keys only")
	hermesSyncCmd.Flags().BoolVar(&hermesDryRun, "dry-run", false, "print the auth-key NAMES and target model without writing anything")
	hermesEnsureCmd.Flags().BoolVar(&hermesNoModel, "no-model", false, "skip model line during the sync step")
	hermesEnsureCmd.Flags().BoolVar(&hermesDryRun, "dry-run", false, "report what would be synced without writing")

	hermesCmd.AddCommand(hermesAuditCmd)
	hermesCmd.AddCommand(hermesSyncCmd)
	hermesCmd.AddCommand(hermesEnsureCmd)
	rootCmd.AddCommand(hermesCmd)
}

// resetHermesFlags is invoked by tests between RunE drives.
func resetHermesFlags() {
	hermesNode = ""
	hermesAll = false
	hermesJSON = false
	hermesQuiet = false
	hermesTimeout = 30 * time.Second
	hermesHermesDir = "~/.hermes"
	hermesNodesPath = ""
	hermesNoModel = false
	hermesDryRun = false
}

// ── target resolution ─────────────────────────────────────────────────

func resolveHermesTargets() ([]osh.Target, error) {
	if hermesNode != "" && hermesAll {
		return nil, fmt.Errorf("hermes: --node and --all are mutually exclusive")
	}
	if hermesNode != "" {
		t, err := osh.ParseTarget(hermesNode)
		if err != nil {
			return nil, fmt.Errorf("hermes: parse --node: %w", err)
		}
		return []osh.Target{t}, nil
	}
	reg, err := nodes.Load(hermesNodesPath)
	if err != nil {
		return nil, fmt.Errorf("hermes: load nodes: %w", err)
	}
	all := reg.All()
	if len(all) == 0 {
		return nil, fmt.Errorf("hermes: empty nodes registry")
	}
	out := make([]osh.Target, 0, len(all))
	for _, n := range all {
		t, err := osh.ParseTarget(n.String())
		if err != nil {
			return nil, fmt.Errorf("hermes: parse %q: %w", n.String(), err)
		}
		out = append(out, t)
	}
	return out, nil
}

// ── audit ─────────────────────────────────────────────────────────────

func runHermesAudit(cmd *cobra.Command, _ []string) error {
	if !hermesAll && hermesNode == "" {
		// audit defaults to all
		hermesAll = true
	}
	targets, err := resolveHermesTargets()
	if err != nil {
		return err
	}

	logger, err := openHermesLogger()
	if err != nil {
		return err
	}
	defer logger.Close()

	dialer := newDialer(osh.WithConnectTimeout(hermesTimeout))
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	results := probeAll(parent, dialer, targets)
	for _, r := range results {
		_ = logger.Action(log.Event{
			Node:       r.Target,
			Phase:      "hermes",
			Action:     "audit",
			Result:     hermesAuditResultFn(r),
			DurationMs: r.DurationMs,
			StderrTail: tailBytes(r.Error, 512),
		})
	}

	if hermesQuiet {
		return nil
	}
	if hermesJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(results)
	}
	renderAuditMarkdown(cmd.OutOrStdout(), results)
	return nil
}

func hermesAuditResultFn(s HermesStatus) log.Result {
	switch {
	case s.Error != "":
		return log.ResultError
	case !s.Configured:
		return log.ResultWarn
	default:
		return log.ResultOK
	}
}

func probeAll(ctx context.Context, dialer Dialer, targets []osh.Target) []HermesStatus {
	results := make([]HermesStatus, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target osh.Target) {
			defer wg.Done()
			results[idx] = probeOne(ctx, dialer, target)
		}(i, t)
	}
	wg.Wait()
	return results
}

func probeOne(ctx context.Context, dialer Dialer, target osh.Target) HermesStatus {
	start := time.Now()
	label := target.User + "@" + target.Host
	dialCtx, cancel := context.WithTimeout(ctx, hermesTimeout)
	defer cancel()

	client, err := dialer.Dial(dialCtx, target, osh.WithConnectTimeout(hermesTimeout))
	if err != nil {
		return HermesStatus{Target: label, ExitCode: -1, Error: err.Error(),
			DurationMs: time.Since(start).Milliseconds()}
	}
	defer client.Close()

	res, runErr := client.Run(dialCtx, hermesStatusCmd())
	dur := time.Since(start).Milliseconds()
	parsed := ParseHermesStatus(res.Stdout)
	parsed.Target = label
	parsed.ExitCode = res.ExitCode
	parsed.DurationMs = dur
	if runErr != nil && parsed.Error == "" {
		parsed.Error = runErr.Error()
	}
	if !parsed.HermesAvailable && parsed.Error == "" {
		parsed.Error = "hermes binary not on PATH or unavailable"
	}
	parsed.Raw = tailBytes(res.Stdout, 4096)
	return parsed
}

// hermesStatusCmd is the remote shell snippet used by audit + sync verify.
// It restores the brew + worker-local PATH so the `hermes` binary
// installed by `fleetctl bootstrap-worker` (~/.local/bin) or by brew
// (/opt/homebrew/bin, /usr/local/bin) is resolvable inside a
// non-interactive ssh session.
func hermesStatusCmd() string {
	return `export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`hermes status 2>&1 || echo "HERMES_UNAVAILABLE"`
}

// ── status parser ─────────────────────────────────────────────────────

// ParseHermesStatus extracts model/provider/keys from the formatted output
// of `hermes status`. It tolerates terminal box-drawing characters and
// truncation.
func ParseHermesStatus(out string) HermesStatus {
	s := HermesStatus{KeysPresent: []string{}}
	if out == "" {
		return s
	}
	lower := strings.ToLower(out)
	unavailable := strings.Contains(out, "HERMES_UNAVAILABLE") ||
		strings.Contains(lower, "command not found")
	noProvider := strings.Contains(lower, "no llm provider")

	keys := map[string]bool{}
	inKeysSection := false

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(strings.ReplaceAll(raw, "│", ""))
		if line == "" {
			continue
		}
		if strings.Contains(line, "API Keys") {
			inKeysSection = true
			continue
		}
		if strings.HasPrefix(line, "◆") && !strings.Contains(line, "API Keys") {
			inKeysSection = false
		}
		lc := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lc, "model:"):
			s.Model = strings.TrimSpace(line[len("Model:"):])
		case strings.HasPrefix(lc, "provider:"):
			s.Provider = strings.TrimSpace(line[len("Provider:"):])
		case inKeysSection:
			present := strings.Contains(line, "✓")
			absent := strings.Contains(line, "✗")
			if present || absent {
				name := line
				for _, sep := range []string{"✓", "✗"} {
					if i := strings.Index(name, sep); i >= 0 {
						name = name[:i]
						break
					}
				}
				name = strings.TrimSpace(name)
				if name != "" {
					keys[name] = present
				}
			}
		}
	}
	for k, v := range keys {
		if v {
			s.KeysPresent = append(s.KeysPresent, k)
		}
	}
	// Stable ordering for diffs / tests.
	sortStrings(s.KeysPresent)

	s.ProviderHasKey = s.Provider != "" && keys[s.Provider]
	s.HermesAvailable = !unavailable
	s.Configured = !unavailable && !noProvider && s.Model != "" && s.Provider != "" && s.ProviderHasKey
	s.NeedsSetup = unavailable || noProvider || (s.Provider != "" && !s.ProviderHasKey)
	return s
}

// ── sync ──────────────────────────────────────────────────────────────

func runHermesSync(cmd *cobra.Command, _ []string) error {
	if !hermesAll && hermesNode == "" {
		hermesAll = true
	}
	targets, err := resolveHermesTargets()
	if err != nil {
		return err
	}

	hermesDir, err := expandHomePath(hermesHermesDir)
	if err != nil {
		return fmt.Errorf("hermes: expand hermes-dir: %w", err)
	}

	authEnv, err := readGatewayAuthEnv(hermesDir)
	if err != nil {
		return fmt.Errorf("hermes: read gateway .env: %w", err)
	}
	if len(authEnv) == 0 {
		return fmt.Errorf("hermes: gateway %s/.env has no recognized auth keys", hermesDir)
	}

	var model string
	if !hermesNoModel {
		model, err = readGatewayModel(hermesDir)
		if err != nil {
			return fmt.Errorf("hermes: read gateway model: %w", err)
		}
	}

	keysPushed := sortedKeys(authEnv)

	if hermesDryRun {
		// Don't dial workers in dry-run; just print what would happen.
		return emitDryRun(cmd.OutOrStdout(), targets, keysPushed, model)
	}

	logger, err := openHermesLogger()
	if err != nil {
		return err
	}
	defer logger.Close()

	dialer := newDialer(osh.WithConnectTimeout(hermesTimeout))
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	results := syncAll(parent, dialer, targets, authEnv, model)
	for _, r := range results {
		_ = logger.Action(log.Event{
			Node:       r.Target,
			Phase:      "hermes",
			Action:     "sync",
			Result:     syncResultLog(r),
			DurationMs: r.DurationMs,
			StderrTail: tailBytes(r.Error, 512),
		})
	}

	if hermesQuiet {
		return finalSyncErr(results)
	}
	if hermesJSON {
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(results)
	} else {
		renderSyncMarkdown(cmd.OutOrStdout(), results)
	}
	return finalSyncErr(results)
}

func syncResultLog(r SyncResult) log.Result {
	switch {
	case r.ExitCode == 0 && r.Configured:
		return log.ResultOK
	case r.Error != "":
		return log.ResultError
	default:
		return log.ResultWarn
	}
}

func finalSyncErr(results []SyncResult) error {
	bad := 0
	for _, r := range results {
		if r.ExitCode != 0 || r.Error != "" {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("hermes sync: %d/%d nodes failed", bad, len(results))
	}
	return nil
}

func syncAll(ctx context.Context, dialer Dialer, targets []osh.Target,
	auth map[string]string, model string) []SyncResult {
	out := make([]SyncResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target osh.Target) {
			defer wg.Done()
			out[idx] = syncOne(ctx, dialer, target, auth, model)
		}(i, t)
	}
	wg.Wait()
	return out
}

func syncOne(ctx context.Context, dialer Dialer, target osh.Target,
	auth map[string]string, model string) SyncResult {
	start := time.Now()
	label := target.User + "@" + target.Host
	r := SyncResult{Target: label, ExitCode: -1, KeysPushed: sortedKeys(auth)}

	if model != "" {
		r.ModelApplied = model
	}

	dialCtx, cancel := context.WithTimeout(ctx, hermesTimeout*2)
	defer cancel()

	client, err := dialer.Dial(dialCtx, target, osh.WithConnectTimeout(hermesTimeout))
	if err != nil {
		r.Error = err.Error()
		r.DurationMs = time.Since(start).Milliseconds()
		return r
	}
	defer client.Close()

	// Build the credential payload + remote script. The payload is a
	// NUL-terminated key=value stream so values may contain literal
	// newlines without confusing the splitter. The remote script
	// reads stdin, splits on NUL, validates each key against the
	// same allow-list, and merges into ~/.hermes/.env atomically.
	payload := buildAuthPayload(auth)
	script := buildSyncScript(sortedKeys(auth), model)

	rc, ok := client.(RemoteClientWith)
	if !ok {
		r.Error = "ssh client does not support RunWith stdin"
		r.DurationMs = time.Since(start).Milliseconds()
		return r
	}
	res, runErr := rc.RunWith(dialCtx, script, osh.RunOpts{Stdin: strings.NewReader(payload)})
	r.ExitCode = res.ExitCode
	if runErr != nil {
		r.Error = runErr.Error()
	} else if res.ExitCode != 0 {
		r.Error = strings.TrimSpace(tailBytes(res.Stderr, 512))
	}

	// Verify by running `hermes status` post-sync.
	verifyRes, _ := client.Run(dialCtx, hermesStatusCmd())
	r.AfterStatus = tailBytes(verifyRes.Stdout, 4096)
	parsed := ParseHermesStatus(verifyRes.Stdout)
	r.Configured = parsed.Configured

	r.DurationMs = time.Since(start).Milliseconds()
	return r
}

// RemoteClientWith extends the local RemoteClient interface with stdin
// support. The real *ssh.Client satisfies this; test fakes may opt in.
type RemoteClientWith interface {
	RemoteClient
	RunWith(ctx context.Context, cmd string, opts osh.RunOpts) (osh.Result, error)
}

// buildAuthPayload serializes the allow-listed keys into a NUL-terminated
// "KEY=VALUE\0KEY2=VALUE2\0…" stream. Values are passed through verbatim;
// the remote script wraps each value in single quotes when writing so
// shell parsing of the resulting .env is unambiguous regardless of the
// value content.
func buildAuthPayload(auth map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(auth) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(auth[k])
		b.WriteByte(0)
	}
	return b.String()
}

// buildSyncScript composes the per-worker shell script that merges the
// auth keys into ~/.hermes/.env and (optionally) updates the model line in
// config.yaml. The allow-list pattern and the literal model value are the
// only places where Go-side values appear in the shell — both are
// constructed from the closed-set `hermesAuthKeys` and a value we read
// from the gateway's own config.yaml, never from a user-supplied flag.
func buildSyncScript(keysOrder []string, model string) string {
	// Per-key escape so each key is regex-literal but the `|` alternation
	// stays meta. (Naively QuoteMeta-ing the joined string would escape
	// the alternation itself.)
	escaped := make([]string, len(keysOrder))
	for i, k := range keysOrder {
		escaped[i] = regexp.QuoteMeta(k)
	}
	pattern := "^(" + strings.Join(escaped, "|") + ")="

	// Shell-quoted model literal (or empty string for no-op).
	modelQuoted := shellSingleQuote(model)

	return fmt.Sprintf(`set -e
cd ~/.hermes 2>/dev/null || { mkdir -p ~/.hermes && cd ~/.hermes; }
umask 077
# Read NUL-terminated KEY=VAL payload from ssh stdin.
PAYLOAD=$(cat | tr '\0' '\n')
# Strip any prior allow-listed lines, then append the new ones.
if [ -f .env ]; then
  grep -vE %s .env > .env.new 2>/dev/null || : > .env.new
else
  : > .env.new
fi
while IFS= read -r line; do
  [ -z "$line" ] && continue
  key=${line%%%%=*}
  val=${line#*=}
  # Single-quote-wrap the value; escape any embedded single quotes.
  esc=$(printf '%%s' "$val" | sed "s/'/'\\\\''/g")
  printf "%%s='%%s'\n" "$key" "$esc" >> .env.new
done <<< "$PAYLOAD"
chmod 600 .env.new
mv .env.new .env
if [ -n %s ] && [ -f config.yaml ]; then
  awk -v m=%s '
    BEGIN { applied=0; in_block=0 }
    /^model:[ \t]*$/ { in_block=1; print; next }
    /^model:[ \t]+/  {
      if (!applied) { print "model: " m; applied=1; next }
    }
    in_block && /^[ \t]+default:/ {
      if (!applied) {
        sub(/default:.*/, "default: " m)
        applied=1; print; next
      }
    }
    /^[^ \t#]/ && NR > 1 { in_block=0 }
    { print }
    END { if (!applied) print "model: " m }
  ' config.yaml > config.yaml.new
  mv config.yaml.new config.yaml
  chmod 600 config.yaml
fi
echo "HERMES_SYNC_OK"
`, shellSingleQuote(pattern), modelQuoted, modelQuoted)
}

// shellSingleQuote returns the shell-safe single-quoted form of s. Empty
// strings are returned as "''", which is how POSIX shells represent an
// empty literal.
func shellSingleQuote(s string) string {
	if s == "" {
		return "''"
	}
	// Replace ' with '"'"' (close, literal-quote, open).
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ── gateway config readers ────────────────────────────────────────────

// readGatewayAuthEnv parses ~/.hermes/.env (plain text) and returns only
// the allow-listed auth keys. Quotes around values are stripped if
// outermost characters match (single or double).
func readGatewayAuthEnv(hermesDir string) (map[string]string, error) {
	path := filepath.Join(hermesDir, ".env")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, k := range hermesAuthKeys {
		want[k] = true
	}
	out := map[string]string{}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[0] == v[len(v)-1] {
			v = v[1 : len(v)-1]
		}
		if want[k] && v != "" {
			out[k] = v
		}
	}
	return out, nil
}

// readGatewayModel returns the configured model from ~/.hermes/config.yaml.
// Handles two YAML shapes seen in the wild:
//
//	(a) flat:      `model: nvidia/...`
//	(b) nested:    `model:\n  default: nvidia/...`        ← gateway's actual shape
//
// For the nested form we read the `default:` sub-key, since that's where
// `hermes status` sources its "Model:" display value from. Returns
// ("", nil) if no model line is present — callers treat that as "skip
// model sync".
func readGatewayModel(hermesDir string) (string, error) {
	path := filepath.Join(hermesDir, "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "model:") {
			continue
		}
		// shape (a): inline value on the same line
		rest := strings.TrimSpace(strings.TrimPrefix(line, "model:"))
		if rest != "" {
			return unquoteYAML(rest), nil
		}
		// shape (b): walk subsequent indented lines looking for `default:`.
		// Stop at the next top-level (column-0 non-space) key.
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			trim := strings.TrimSpace(next)
			if trim == "" || strings.HasPrefix(trim, "#") {
				continue
			}
			indent := len(next) - len(strings.TrimLeft(next, " \t"))
			if indent == 0 {
				break // back at top-level, stop
			}
			if strings.HasPrefix(trim, "default:") {
				val := strings.TrimSpace(strings.TrimPrefix(trim, "default:"))
				return unquoteYAML(val), nil
			}
		}
		break
	}
	return "", nil
}

// unquoteYAML strips a single layer of matching single or double quotes.
func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[0] == s[len(s)-1] {
		return s[1 : len(s)-1]
	}
	return s
}

// ── ensure-config ─────────────────────────────────────────────────────

func runHermesEnsure(cmd *cobra.Command, _ []string) error {
	if !hermesAll && hermesNode == "" {
		hermesAll = true
	}
	targets, err := resolveHermesTargets()
	if err != nil {
		return err
	}

	logger, err := openHermesLogger()
	if err != nil {
		return err
	}
	defer logger.Close()

	dialer := newDialer(osh.WithConnectTimeout(hermesTimeout))
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	audit := probeAll(parent, dialer, targets)
	needsSync := make([]osh.Target, 0, len(targets))
	for i, a := range audit {
		if a.NeedsSetup {
			needsSync = append(needsSync, targets[i])
		}
	}

	hermesDir, err := expandHomePath(hermesHermesDir)
	if err != nil {
		return fmt.Errorf("hermes: expand hermes-dir: %w", err)
	}
	authEnv, err := readGatewayAuthEnv(hermesDir)
	if err != nil {
		return fmt.Errorf("hermes: read gateway .env: %w", err)
	}
	model := ""
	if !hermesNoModel {
		if m, err := readGatewayModel(hermesDir); err == nil {
			model = m
		}
	}

	var syncResults []SyncResult
	if len(needsSync) > 0 && !hermesDryRun {
		if len(authEnv) == 0 {
			return fmt.Errorf("hermes: %d nodes need sync but gateway .env has no auth keys", len(needsSync))
		}
		syncResults = syncAll(parent, dialer, needsSync, authEnv, model)
		for _, r := range syncResults {
			_ = logger.Action(log.Event{
				Node: r.Target, Phase: "hermes", Action: "ensure-sync",
				Result: syncResultLog(r), DurationMs: r.DurationMs,
				StderrTail: tailBytes(r.Error, 512),
			})
		}
	}
	for _, a := range audit {
		_ = logger.Action(log.Event{
			Node: a.Target, Phase: "hermes", Action: "ensure-audit",
			Result: hermesAuditResultFn(a), DurationMs: a.DurationMs,
			StderrTail: tailBytes(a.Error, 512),
		})
	}

	if hermesQuiet {
		return ensureFinalErr(audit, syncResults)
	}

	if hermesJSON {
		body := map[string]any{
			"audit":   audit,
			"synced":  syncResults,
			"dry_run": hermesDryRun,
			"model":   model,
			"keys":    sortedKeys(authEnv),
		}
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(body)
	} else {
		renderEnsureMarkdown(cmd.OutOrStdout(), audit, syncResults, needsSync, model, sortedKeys(authEnv))
	}
	return ensureFinalErr(audit, syncResults)
}

func ensureFinalErr(audit []HermesStatus, sync []SyncResult) error {
	stillBad := 0
	bySync := map[string]SyncResult{}
	for _, s := range sync {
		bySync[s.Target] = s
	}
	for _, a := range audit {
		if !a.NeedsSetup {
			continue
		}
		s, ok := bySync[a.Target]
		if !ok || s.Error != "" || s.ExitCode != 0 || !s.Configured {
			stillBad++
		}
	}
	if stillBad > 0 {
		return fmt.Errorf("hermes ensure-config: %d/%d nodes still need setup", stillBad, len(audit))
	}
	return nil
}

// ── rendering ─────────────────────────────────────────────────────────

func renderAuditMarkdown(w interface{ Write([]byte) (int, error) }, audit []HermesStatus) {
	fmt.Fprintln(w, "| node | configured | model | provider | keys present | error |")
	fmt.Fprintln(w, "|------|------------|-------|----------|---------------|-------|")
	for _, a := range audit {
		state := "yes"
		if !a.Configured {
			if a.NeedsSetup {
				state = "NEEDS SETUP"
			} else {
				state = "no"
			}
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s |\n",
			a.Target, state, dashIfEmpty(a.Model), dashIfEmpty(a.Provider),
			strings.Join(a.KeysPresent, ", "), dashIfEmpty(a.Error))
	}
}

func renderSyncMarkdown(w interface{ Write([]byte) (int, error) }, sync []SyncResult) {
	fmt.Fprintln(w, "| node | exit | configured | keys pushed | model | error |")
	fmt.Fprintln(w, "|------|------|------------|-------------|-------|-------|")
	for _, r := range sync {
		state := "yes"
		if !r.Configured {
			state = "no"
		}
		fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s |\n",
			r.Target, r.ExitCode, state,
			strings.Join(r.KeysPushed, ", "), dashIfEmpty(r.ModelApplied),
			dashIfEmpty(r.Error))
	}
}

func renderEnsureMarkdown(w interface{ Write([]byte) (int, error) },
	audit []HermesStatus, sync []SyncResult, needsSync []osh.Target,
	model string, keys []string) {

	ok, bad := 0, 0
	for _, a := range audit {
		if a.NeedsSetup {
			bad++
		} else {
			ok++
		}
	}
	fmt.Fprintf(w, "audit: %d ok, %d need setup (of %d total)\n",
		ok, bad, len(audit))
	if hermesDryRun {
		fmt.Fprintf(w, "dry-run: would push keys=%v model=%s to %d nodes\n",
			keys, dashIfEmpty(model), len(needsSync))
		return
	}
	if len(sync) > 0 {
		fmt.Fprintln(w, "\nsync results:")
		renderSyncMarkdown(w, sync)
	} else {
		fmt.Fprintln(w, "\nnothing to sync.")
	}
}

func emitDryRun(w interface{ Write([]byte) (int, error) },
	targets []osh.Target, keys []string, model string) error {
	fmt.Fprintf(w, "dry-run · targets=%d · keys=%v · model=%s\n",
		len(targets), keys, dashIfEmpty(model))
	for _, t := range targets {
		fmt.Fprintf(w, "  %s@%s\n", t.User, t.Host)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// openHermesLogger expands the log path and opens a JSONL logger.
// Mirrors the patterns used by delegate/health (openLogger override is
// shared across the package for test injection).
func openHermesLogger() (*log.Logger, error) {
	p, err := expandHomePath(hermesLogPath)
	if err != nil {
		return nil, fmt.Errorf("hermes: expand log path: %w", err)
	}
	return openLogger(p)
}

// ── skill registration ───────────────────────────────────────────────

func hermesSkill() Skill {
	return Skill{
		Name:    "hermes",
		Trigger: "/hermes",
		Summary: "Audit and reconcile per-node hermes-agent config (LLM auth keys + model).",
		Args: []SkillArg{
			{Name: "audit", Description: "Read-only per-node status. Add --json or --node user@host."},
			{Name: "sync", Description: "Push gateway auth keys + model to selected workers. --all, --node, --no-model, --dry-run."},
			{Name: "ensure-config", Description: "Audit, then sync any nodes that need setup. Idempotent."},
		},
		Examples: []SkillExample{
			{Description: "See which nodes are configured.",
				Command: "fleetctl hermes audit"},
			{Description: "Make every worker match gateway (keys + model).",
				Command: "fleetctl hermes ensure-config"},
			{Description: "Sync just one node.",
				Command: "fleetctl hermes sync --node hermes@claw1.local"},
		},
	}
}

func init() { registerSkill("hermes", hermesSkill) }
