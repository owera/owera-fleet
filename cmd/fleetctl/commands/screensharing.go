// screensharing.go implements `fleetctl screensharing` — probe and toggle
// macOS Screen Sharing (the system launchd job
// /System/Library/LaunchDaemons/com.apple.screensharing.plist, which is
// what the System Settings → "Screen Sharing" toggle controls) across the
// fleet.
//
// # Scope
//
// `fleetctl screensharing status` reads ~/.hermes/nodes.txt and SSH-probes
// every worker (plus the gateway by default) for whether the screen-sharing
// daemon is currently listening on TCP/5900. This probe is intentionally
// non-privileged — we look for a listening socket instead of asking
// `launchctl` about a system-domain job, which would require sudo on
// modern macOS and is the same wall that hit the manual claw0 enablement
// attempts on 2026-05-21 (see hermes-setup STATE.md entries S386/S387).
//
// `fleetctl screensharing enable` and `disable` issue the canonical
// launchctl load/unload calls. Both require root, and the worker `hermes`
// user is non-admin by design. We attempt `sudo -n` (non-interactive); if
// it fails, the report prints the exact remediation command and points at
// `fleetctl screensharing setup-sudoers` to print the NOPASSWD stanza an
// admin can drop into /etc/sudoers.d/.
//
// `fleetctl screensharing setup-sudoers` is a pure-printer — it emits the
// sudoers stanza on stdout. It does NOT install the file (the worker
// fleetctl process itself cannot, by design). The operator pipes the
// output through `sudo tee /etc/sudoers.d/fleetctl-screensharing` from an
// admin session on each target host. Once installed, subsequent
// `fleetctl screensharing enable --all` runs are passwordless.
//
// # Why a listening-port probe instead of `launchctl list`
//
// `launchctl list com.apple.screensharing` only sees the *current* user's
// launchd context. The screen-sharing daemon runs in the system context,
// so a user-context `launchctl list` always reports "not found" even on
// a host where Screen Sharing is enabled. `launchctl print
// system/com.apple.screensharing` would work but requires sudo. The TCP
// listening check is sudo-free and matches what System Settings reports.

package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

var (
	scrSharingNodesPath      = "~/.hermes/nodes.txt"
	scrSharingLogPath        = "~/.hermes/logs/owera-screensharing.jsonl"
	scrSharingSSHTimeout     time.Duration
	scrSharingActionTimeout  time.Duration
	scrSharingIncludeGateway bool
	scrSharingFilter         []string
	scrSharingQuiet          bool
	scrSharingJSON           bool
)

// screenSharingPlist is the canonical system LaunchDaemon path. It is the
// same plist on every supported macOS release (Big Sur through Sequoia).
const screenSharingPlist = "/System/Library/LaunchDaemons/com.apple.screensharing.plist"

// vncPort is the TCP port screen sharing listens on. macOS Screen Sharing
// is VNC-compatible and always binds 5900 when enabled.
const vncPort = 5900

// sudoersStanzaUser is the worker-side user whose fleetctl invocations
// need passwordless launchctl. Stays in sync with hermes-setup's bootstrap
// (the worker user is `hermes`).
const sudoersStanzaUser = "hermes"

var scrSharingCmd = &cobra.Command{
	Use:   "screensharing",
	Short: "Probe + toggle macOS Screen Sharing across the fleet",
	Long: `screensharing probes ~/.hermes/nodes.txt (plus the gateway by default)
for whether the system Screen Sharing daemon is listening on TCP/5900,
and toggles it on/off via launchctl load/unload of
` + screenSharingPlist + `.

With no --node filter, every entry in nodes.txt is targeted (plus the
gateway unless --include-gateway=false).

Probing is non-privileged (looks for a listening socket on 5900).
Enable / disable require root; both are issued via 'sudo -n launchctl'.
For passwordless operation across the fleet, install the NOPASSWD
sudoers stanza on each host (admin-side, one-time):

    fleetctl screensharing setup-sudoers | ssh admin@host 'sudo tee /etc/sudoers.d/fleetctl-screensharing >/dev/null && sudo chmod 440 /etc/sudoers.d/fleetctl-screensharing'

Once installed, 'fleetctl screensharing enable' fans out hands-off.

This does NOT configure VNC access privileges (Apple Remote Desktop
'kickstart -privs' flags) — Screen Sharing inherits whatever per-user
access the host already has wired up via System Settings.`,
	Example: `  fleetctl screensharing status                    # report all hosts (no mutations)
  fleetctl screensharing status --json             # machine-readable
  fleetctl screensharing enable --node hermes@claw1.local
  fleetctl screensharing enable                     # fan out to every host in nodes.txt
  fleetctl screensharing disable --node hermes@claw1.local
  fleetctl screensharing setup-sudoers              # print sudoers stanza`,
}

var scrSharingStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report Screen Sharing state per host (no mutations)",
	RunE:  runScrSharingStatus,
}

var scrSharingEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable Screen Sharing on the targeted host(s) (sudo required)",
	RunE:  runScrSharingEnable,
}

var scrSharingDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable Screen Sharing on the targeted host(s) (sudo required)",
	RunE:  runScrSharingDisable,
}

var scrSharingSudoersCmd = &cobra.Command{
	Use:   "setup-sudoers",
	Short: "Print the NOPASSWD sudoers stanza for passwordless enable/disable",
	RunE:  runScrSharingSudoers,
}

func init() {
	for _, c := range []*cobra.Command{scrSharingStatusCmd, scrSharingEnableCmd, scrSharingDisableCmd} {
		c.Flags().DurationVar(&scrSharingSSHTimeout, "ssh-timeout", 30*time.Second, "per-host SSH probe timeout")
		c.Flags().BoolVar(&scrSharingIncludeGateway, "include-gateway", true, "also operate on the gateway (set --include-gateway=false to skip)")
		c.Flags().StringSliceVar(&scrSharingFilter, "node", nil, "restrict to this user@host (repeatable; default all entries in nodes.txt)")
		c.Flags().BoolVar(&scrSharingQuiet, "quiet", false, "suppress stdout summary; JSONL log unchanged")
	}
	scrSharingStatusCmd.Flags().BoolVar(&scrSharingJSON, "json", false, "emit one JSON object per host to stdout (status only)")
	// --all is offered on enable/disable as syntactic sugar for "every node
	// in nodes.txt" — without it operators have to repeat --node N times.
	for _, c := range []*cobra.Command{scrSharingEnableCmd, scrSharingDisableCmd} {
		c.Flags().DurationVar(&scrSharingActionTimeout, "action-timeout", 30*time.Second, "per-host launchctl ceiling")
	}

	scrSharingCmd.AddCommand(scrSharingStatusCmd)
	scrSharingCmd.AddCommand(scrSharingEnableCmd)
	scrSharingCmd.AddCommand(scrSharingDisableCmd)
	scrSharingCmd.AddCommand(scrSharingSudoersCmd)
	rootCmd.AddCommand(scrSharingCmd)
	registerSkill("screensharing", scrSharingSkill)
}

// scrSharingState is one host's probe result.
type scrSharingState struct {
	Target     string // "gateway" or "user@host"
	Host       string // short label
	Listening  bool   // true when TCP/5900 is bound
	Action     string // "noop" | "enable" | "disable" | "probe"
	Result     string // "ok" | "already" | "sudo-required" | "error"
	StderrTail string // last few lines from launchctl / probe (when relevant)
	Err        string // probe-layer error
}

// HasError returns true when the host probe itself failed (distinct from
// the daemon being off, which is a valid "ok" result).
func (s scrSharingState) HasError() bool { return s.Err != "" }

// --- run* functions ---

func runScrSharingStatus(cmd *cobra.Command, _ []string) error {
	logger, err := scrSharingOpenLogger()
	if err != nil {
		return err
	}
	defer logger.Close()
	targets, err := scrSharingResolveTargets()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("screensharing status: no targets")
	}
	dialer := newDialer(osh.WithConnectTimeout(scrSharingSSHTimeout))
	reports := make([]scrSharingState, 0, len(targets))
	for _, t := range targets {
		s := scrSharingProbeOne(cmd.Context(), dialer, t, scrSharingSSHTimeout)
		s.Action = "probe"
		s.Result = scrSharingProbeResultLabel(s)
		reports = append(reports, s)
		_ = logger.Action(log.Event{
			Node: s.Host, Phase: "screensharing",
			Action: "probe", Result: scrSharingLogResult(s),
			StderrTail: scrSharingProbeTail(s),
		})
	}
	if !scrSharingQuiet {
		if scrSharingJSON {
			fmt.Fprint(cmd.OutOrStdout(), scrSharingJSONReport(reports))
		} else {
			fmt.Fprint(cmd.OutOrStdout(), scrSharingMarkdownTable(reports, "status"))
		}
	}
	if scrSharingAnyError(reports) {
		cmd.SilenceUsage = true
		return fmt.Errorf("screensharing status: %d host(s) probe-failed", scrSharingCountErrors(reports))
	}
	return nil
}

func runScrSharingEnable(cmd *cobra.Command, _ []string) error {
	return runScrSharingToggle(cmd, true)
}

func runScrSharingDisable(cmd *cobra.Command, _ []string) error {
	return runScrSharingToggle(cmd, false)
}

// runScrSharingToggle is shared between enable and disable; the only
// difference is the launchctl verb and the post-mutation "already in
// desired state" check polarity.
func runScrSharingToggle(cmd *cobra.Command, on bool) error {
	logger, err := scrSharingOpenLogger()
	if err != nil {
		return err
	}
	defer logger.Close()
	targets, err := scrSharingResolveTargets()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("screensharing: no targets")
	}
	dialer := newDialer(osh.WithConnectTimeout(scrSharingSSHTimeout))
	reports := make([]scrSharingState, 0, len(targets))
	failures := 0

	verbWord := "enable"
	if !on {
		verbWord = "disable"
	}

	for _, t := range targets {
		pre := scrSharingProbeOne(cmd.Context(), dialer, t, scrSharingSSHTimeout)
		// Idempotency: skip the mutation entirely when the host is already
		// in the desired state. Saves a sudo prompt and keeps the audit log
		// quiet on repeat runs.
		if pre.Listening == on && !pre.HasError() {
			pre.Action = verbWord
			pre.Result = "already"
			reports = append(reports, pre)
			_ = logger.Action(log.Event{
				Node: pre.Host, Phase: "screensharing",
				Action: verbWord, Result: log.ResultOK,
				StderrTail: "already in desired state (listening=" + fmt.Sprint(pre.Listening) + ")",
			})
			continue
		}
		if pre.HasError() {
			pre.Action = verbWord
			pre.Result = "error"
			reports = append(reports, pre)
			failures++
			_ = logger.Action(log.Event{
				Node: pre.Host, Phase: "screensharing",
				Action: verbWord, Result: log.ResultError,
				StderrTail: pre.Err,
			})
			continue
		}

		start := time.Now()
		runOut, runErr := scrSharingRunToggle(cmd.Context(), dialer, t, on, scrSharingActionTimeout)
		dur := time.Since(start).Round(time.Millisecond)
		st := pre
		st.Action = verbWord
		st.StderrTail = scrSharingActionTail(runOut, runErr)

		if runErr != nil {
			st.Result = scrSharingClassifyError(runOut, runErr)
			failures++
			_ = logger.Action(log.Event{
				Node: st.Host, Phase: "screensharing",
				Action: verbWord, Result: log.ResultError, DurationMs: dur.Milliseconds(),
				StderrTail: st.StderrTail,
			})
			reports = append(reports, st)
			continue
		}

		// Re-probe so the summary reflects post-mutation state.
		post := scrSharingProbeOne(cmd.Context(), dialer, t, scrSharingSSHTimeout)
		post.Action = verbWord
		switch {
		case post.HasError():
			post.Result = "error"
			failures++
		case post.Listening == on:
			post.Result = "ok"
		default:
			// launchctl exited 0 but daemon didn't end up in the requested
			// state. This is the surprising case — log it as an error so an
			// operator knows the toggle didn't stick.
			post.Result = "mismatch"
			failures++
		}
		_ = logger.Action(log.Event{
			Node: post.Host, Phase: "screensharing",
			Action: verbWord, Result: scrSharingLogResultFromResult(post.Result),
			DurationMs: dur.Milliseconds(),
			StderrTail: scrSharingActionTail(runOut, runErr),
		})
		reports = append(reports, post)
	}

	if !scrSharingQuiet {
		fmt.Fprint(cmd.OutOrStdout(), scrSharingMarkdownTable(reports, verbWord))
	}
	if failures > 0 {
		cmd.SilenceUsage = true
		return fmt.Errorf("screensharing %s: %d host(s) failed (see report for remediation)", verbWord, failures)
	}
	return nil
}

func runScrSharingSudoers(cmd *cobra.Command, _ []string) error {
	fmt.Fprint(cmd.OutOrStdout(), scrSharingSudoersStanza())
	return nil
}

// scrSharingSudoersStanza returns the sudoers content an admin should
// install into /etc/sudoers.d/fleetctl-screensharing on every worker host
// so that the `hermes` user can flip the daemon without a password.
func scrSharingSudoersStanza() string {
	return fmt.Sprintf(`# fleetctl screensharing — passwordless toggle for the system Screen Sharing
# LaunchDaemon. Install on each worker host with:
#   fleetctl screensharing setup-sudoers | sudo tee /etc/sudoers.d/fleetctl-screensharing >/dev/null
#   sudo chmod 440 /etc/sudoers.d/fleetctl-screensharing
# Validate with: sudo visudo -c
%s ALL=(root) NOPASSWD: /bin/launchctl load -w %s, /bin/launchctl unload -w %s
`, sudoersStanzaUser, screenSharingPlist, screenSharingPlist)
}

// --- probe + action shell scripts ---

// scrSharingProbeScript is a sudo-free shell snippet that prints a single
// `listening=yes` or `listening=no` line based on whether TCP/5900 is
// bound. We use `lsof -nP -iTCP:5900 -sTCP:LISTEN` (present on every
// supported macOS) and fall back to `netstat -an` so the probe still
// works if lsof is somehow missing.
func scrSharingProbeScript() string {
	return fmt.Sprintf(`port=%d
listening=no
if command -v lsof >/dev/null 2>&1; then
  if lsof -nP -iTCP:$port -sTCP:LISTEN 2>/dev/null | tail -n +2 | grep -q .; then
    listening=yes
  fi
elif command -v netstat >/dev/null 2>&1; then
  if netstat -an -p tcp 2>/dev/null | awk -v p=$port '$NF=="LISTEN" && $4 ~ "\\."p"$" {found=1} END{exit !found}'; then
    listening=yes
  fi
fi
printf 'listening=%%s\n' "$listening"
`, vncPort)
}

// scrSharingToggleScript wraps the launchctl call in `sudo -n` and
// captures stderr so the caller can classify the failure (sudo-required
// vs. plist-missing vs. genuine launchctl error).
func scrSharingToggleScript(on bool) string {
	verb := "load"
	if !on {
		verb = "unload"
	}
	return fmt.Sprintf(`sudo -n /bin/launchctl %s -w %s 2>&1
`, verb, screenSharingPlist)
}

// --- target resolution + SSH execution (mirrors brew.go) ---

func scrSharingResolveTargets() ([]brewTarget, error) {
	nodesPath, err := expandHomePath(scrSharingNodesPath)
	if err != nil {
		return nil, err
	}
	reg, err := nodes.Load(nodesPath)
	if err != nil {
		return nil, fmt.Errorf("screensharing: load nodes: %w", err)
	}
	all := reg.All()
	wanted := map[string]bool{}
	for _, f := range scrSharingFilter {
		wanted[strings.TrimSpace(f)] = true
	}
	out := make([]brewTarget, 0, len(all)+1)
	if scrSharingIncludeGateway && len(scrSharingFilter) == 0 {
		out = append(out, brewTarget{Label: "gateway", Host: shortHostname(), Local: true})
	}
	for _, n := range all {
		if len(wanted) > 0 && !wanted[n.String()] {
			continue
		}
		t, err := osh.ParseTarget(n.String())
		if err != nil {
			return nil, fmt.Errorf("screensharing: parse %s: %w", n.String(), err)
		}
		out = append(out, brewTarget{Label: n.String(), Host: n.Host, Local: false, SSH: t})
	}
	return out, nil
}

func scrSharingProbeOne(ctx context.Context, dialer Dialer, t brewTarget, sshTimeout time.Duration) scrSharingState {
	s := scrSharingState{
		Target: t.Label,
		Host:   t.Host,
	}
	out, err := brewRunOnTarget(ctx, dialer, t, scrSharingProbeScript(), sshTimeout)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	parseScrSharingProbe(out, &s)
	return s
}

// scrSharingRunToggle ssh-invokes `sudo -n launchctl (un)load -w <plist>`
// on the target. Returns the merged stdout+stderr (launchctl writes its
// errors to stderr; sudo writes its prompt-required error to stderr too).
func scrSharingRunToggle(ctx context.Context, dialer Dialer, t brewTarget, on bool, timeout time.Duration) (string, error) {
	return brewRunOnTarget(ctx, dialer, t, scrSharingToggleScript(on), timeout)
}

func parseScrSharingProbe(out string, s *scrSharingState) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		k, v := line[:idx], line[idx+1:]
		if k == "listening" {
			s.Listening = (v == "yes")
		}
	}
}

// scrSharingClassifyError returns a one-word result label based on the
// launchctl/sudo failure shape. The most common failure on a fresh fleet
// is "sudo: a password is required" — we surface that explicitly so the
// operator immediately reaches for `setup-sudoers`.
func scrSharingClassifyError(out string, err error) string {
	tail := strings.ToLower(out + " " + err.Error())
	switch {
	case strings.Contains(tail, "password is required"),
		strings.Contains(tail, "a terminal is required"),
		strings.Contains(tail, "sudo: a password"):
		return "sudo-required"
	case strings.Contains(tail, "no such file"),
		strings.Contains(tail, "could not find specified service"):
		return "plist-missing"
	default:
		return "error"
	}
}

// --- summary rendering + log helpers ---

func scrSharingProbeResultLabel(s scrSharingState) string {
	if s.HasError() {
		return "error"
	}
	if s.Listening {
		return "on"
	}
	return "off"
}

func scrSharingLogResult(s scrSharingState) log.Result {
	if s.HasError() {
		return log.ResultError
	}
	return log.ResultOK
}

func scrSharingLogResultFromResult(r string) log.Result {
	switch r {
	case "ok", "already":
		return log.ResultOK
	case "sudo-required":
		return log.ResultWarn
	default:
		return log.ResultError
	}
}

func scrSharingProbeTail(s scrSharingState) string {
	if s.Err != "" {
		return s.Err
	}
	return fmt.Sprintf("listening=%v", s.Listening)
}

func scrSharingActionTail(out string, err error) string {
	tail := strings.TrimSpace(out)
	if err != nil {
		if tail != "" {
			tail = tail + " | " + err.Error()
		} else {
			tail = err.Error()
		}
	}
	if len(tail) > 800 {
		tail = tail[len(tail)-800:]
	}
	return tail
}

func scrSharingAnyError(reports []scrSharingState) bool {
	return scrSharingCountErrors(reports) > 0
}

func scrSharingCountErrors(reports []scrSharingState) int {
	n := 0
	for _, r := range reports {
		if r.HasError() {
			n++
		}
	}
	return n
}

func scrSharingMarkdownTable(reports []scrSharingState, verb string) string {
	// Stable ordering — gateway first if present, then workers alphabetical
	// by Target. Pre-sort the slice so the rendered table doesn't reshuffle
	// each invocation.
	sorted := append([]scrSharingState(nil), reports...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Target == "gateway" {
			return true
		}
		if sorted[j].Target == "gateway" {
			return false
		}
		return sorted[i].Target < sorted[j].Target
	})
	var b strings.Builder
	fmt.Fprintf(&b, "\n| Host | Listening | %s | Detail |\n", verbColumn(verb))
	b.WriteString("|---|---|---|---|\n")
	for _, r := range sorted {
		listen := "no"
		if r.Listening {
			listen = "yes"
		}
		if r.HasError() {
			listen = "?"
		}
		detail := r.StderrTail
		if detail == "" {
			detail = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.Target, listen, r.Result, scrSharingTrimCell(detail))
	}

	// Remediation footnote for sudo-required rows.
	if scrSharingAnySudoRequired(sorted) {
		b.WriteString("\n**Sudo required** on the rows above. Install the passwordless stanza once per host (admin-side):\n\n")
		b.WriteString("```sh\nfleetctl screensharing setup-sudoers | ssh admin@host 'sudo tee /etc/sudoers.d/fleetctl-screensharing >/dev/null && sudo chmod 440 /etc/sudoers.d/fleetctl-screensharing'\n```\n\n")
		b.WriteString("Then re-run `fleetctl screensharing " + verb + "` to take effect.\n")
	}
	return b.String()
}

// verbColumn returns the appropriate column header for the action column.
// The status subcommand reports "State" (no verb); enable/disable reports
// the verb directly so it's obvious what the row tried to do. Hand-rolled
// rather than reaching for strings.Title (deprecated) since the input
// space is closed (enable / disable / probe / status).
func verbColumn(verb string) string {
	switch verb {
	case "enable":
		return "Enable"
	case "disable":
		return "Disable"
	case "probe":
		return "Probe"
	default:
		return "State"
	}
}

func scrSharingAnySudoRequired(reports []scrSharingState) bool {
	for _, r := range reports {
		if r.Result == "sudo-required" {
			return true
		}
	}
	return false
}

func scrSharingTrimCell(s string) string {
	flat := strings.ReplaceAll(s, "\n", " ")
	flat = strings.ReplaceAll(flat, "|", "/")
	if len(flat) > 160 {
		flat = flat[:160] + "…"
	}
	return strings.TrimSpace(flat)
}

func scrSharingJSONReport(reports []scrSharingState) string {
	var b strings.Builder
	for _, r := range reports {
		// Hand-built JSON keeps us off encoding/json for a one-spot
		// emission and matches the brew/delegate style.
		errField := ""
		if r.Err != "" {
			errField = r.Err
		}
		fmt.Fprintf(&b, `{"target":%q,"host":%q,"listening":%t,"result":%q,"err":%q}`+"\n",
			r.Target, r.Host, r.Listening, r.Result, errField)
	}
	return b.String()
}

func scrSharingOpenLogger() (*log.Logger, error) {
	logPath, err := expandHomePath(scrSharingLogPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("screensharing: mkdir logs dir: %w", err)
	}
	return openLogger(logPath)
}

func scrSharingSkill() Skill {
	return Skill{
		Name:    "screensharing",
		Trigger: "/screensharing",
		Summary: "Probe + toggle macOS Screen Sharing (com.apple.screensharing) across the fleet.",
		Args: []SkillArg{
			{Name: "status", Description: "Report whether TCP/5900 is listening per host (no mutations, no sudo)."},
			{Name: "enable", Description: "Enable Screen Sharing on the targeted host(s). Requires NOPASSWD sudoers stanza (see setup-sudoers)."},
			{Name: "disable", Description: "Disable Screen Sharing on the targeted host(s). Same sudo requirement as enable."},
			{Name: "setup-sudoers", Description: "Print the NOPASSWD sudoers stanza an admin should install into /etc/sudoers.d/fleetctl-screensharing on each host."},
			{Name: "--node USER@HOST", Description: "Restrict to one worker (repeatable; default all entries in nodes.txt)."},
			{Name: "--include-gateway=false", Description: "Skip the gateway leg (default: gateway included)."},
			{Name: "--ssh-timeout DUR", Description: "Per-host SSH probe timeout. Default 30s."},
			{Name: "--action-timeout DUR", Description: "Per-host launchctl ceiling. Default 30s. (enable/disable only)"},
			{Name: "--json", Description: "Emit one JSON object per host to stdout. (status only)"},
			{Name: "--quiet", Description: "Suppress stdout summary; JSONL log unchanged."},
		},
	}
}
