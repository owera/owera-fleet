// brew.go implements `fleetctl brew` — the gateway-side probe + per-host
// Homebrew essentials installer that supersedes the legacy hermes-setup
// `scripts/fleet-brew-baseline.sh`.
//
// # Scope (intentionally narrow)
//
// `fleetctl brew probe` reads ~/.hermes/nodes.txt and SSH-probes every
// worker (plus the gateway by default) for:
//
//   - arch (uname -m)
//   - brew binary path (one of /opt/homebrew/bin/brew or /usr/local/bin/brew)
//   - brew version (if present)
//   - whether brew's shellenv is wired into ~/.local/bin/env
//   - which "essentials" are missing vs present
//
// `fleetctl brew install` then runs `brew install <pkg>` for each host
// that has brew but is missing essentials. Hosts WITHOUT brew get a
// human-readable remediation pointer — installing Homebrew itself
// requires admin sudo, which the `hermes` worker user does not have
// (per the P4 lateral-isolation primitive). For first-time brew install
// on a fresh worker, see hermes-setup `scripts/install-brew-on-worker.sh`
// (admin-side; will be migrated separately when staging-Mac C1
// acceptance is in flight).
//
// # Defaults
//
// Essentials list matches the bash version:
//
//	jq coreutils shellcheck gh wget ripgrep
//
// Some essentials install primary binaries with different names than
// the formula. The `binaryFor` table maps formula → expected binary:
// `coreutils → gtimeout`, `ripgrep → rg`. Used by the probe to confirm
// the binary actually resolves on PATH after install.

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
	brewEssentials      string
	brewNodesPath       = "~/.hermes/nodes.txt"
	brewLogPath         = "~/.hermes/logs/owera-brew.jsonl"
	brewSSHTimeout      time.Duration
	brewInstallTimeout  time.Duration
	brewIncludeGateway  bool
	brewForce           bool
	brewQuiet           bool
	brewFilter          []string
)

// defaultBrewEssentials matches scripts/fleet-brew-baseline.sh. Keeping
// the same baseline so an operator's mental model is portable.
const defaultBrewEssentials = "jq coreutils shellcheck gh wget ripgrep"

// binaryFor maps a Homebrew formula name to the primary binary the
// formula provides. The probe uses `command -v <binary>` after a
// successful install to confirm PATH wiring; for most formulae the
// formula name equals the binary, but coreutils + ripgrep diverge.
var binaryFor = map[string]string{
	"coreutils": "gtimeout",
	"ripgrep":   "rg",
}

var brewCmd = &cobra.Command{
	Use:   "brew",
	Short: "Probe + install Homebrew essentials across the fleet",
	Long: `brew probes ~/.hermes/nodes.txt (plus the gateway by default)
for Homebrew presence, brew version, shellenv wiring, and missing
essentials. The companion 'install' subcommand runs 'brew install <pkg>'
on hosts that have brew but are missing essentials.

Defaults match the legacy bash 'scripts/fleet-brew-baseline.sh':
  essentials  jq coreutils shellcheck gh wget ripgrep
  ssh-timeout 30s
  install-timeout 5m (brew install can be slow on a cold cache)

For first-time Homebrew install on a fresh worker, see the hermes-setup
admin script 'scripts/install-brew-on-worker.sh' — the worker hermes
user is non-admin by design and cannot run the Homebrew installer.`,
	Example: `  fleetctl brew probe                # state report
  fleetctl brew probe --workers-only # skip the gateway
  fleetctl brew install              # install missing essentials on every brew-ready host
  fleetctl brew install --node hermes@claw1.local`,
}

var brewProbeCmd = &cobra.Command{
	Use:   "probe",
	Short: "Report Homebrew state per host (no mutations)",
	RunE:  runBrewProbe,
}

var brewInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install missing essentials on hosts that already have brew",
	RunE:  runBrewInstall,
}

func init() {
	for _, c := range []*cobra.Command{brewProbeCmd, brewInstallCmd} {
		c.Flags().StringVar(&brewEssentials, "essentials", defaultBrewEssentials, "space-separated formula list to probe / install")
		c.Flags().DurationVar(&brewSSHTimeout, "ssh-timeout", 30*time.Second, "per-host SSH probe timeout")
		c.Flags().BoolVar(&brewIncludeGateway, "include-gateway", true, "also probe the gateway (set --include-gateway=false to skip)")
		c.Flags().StringSliceVar(&brewFilter, "node", nil, "restrict to this user@host (repeatable; default all entries in nodes.txt)")
		c.Flags().BoolVar(&brewQuiet, "quiet", false, "suppress stdout summary; JSONL log unchanged")
	}
	brewInstallCmd.Flags().DurationVar(&brewInstallTimeout, "install-timeout", 5*time.Minute, "per-host install ceiling (cold brew caches can take minutes)")
	brewInstallCmd.Flags().BoolVar(&brewForce, "force", false, "re-run install even on hosts with all essentials present (useful after explicit `brew uninstall`)")

	brewCmd.AddCommand(brewProbeCmd)
	brewCmd.AddCommand(brewInstallCmd)
	rootCmd.AddCommand(brewCmd)
	registerSkill("brew", brewSkill)
}

// brewHostReport captures one host's probe result.
type brewHostReport struct {
	Target     string // "gateway" or "user@host"
	Host       string // display short name
	Arch       string // uname -m output or "?"
	BrewPath   string // "/opt/homebrew/bin/brew" or "MISSING" or "probe-failed"
	BrewVer    string // first line of `brew --version` or ""
	ShellenvIn string // "yes" | "no" | "?"
	Missing    []string
	Present    []string
	Err        string
}

// HasBrew returns true when probe found a usable brew binary.
func (r brewHostReport) HasBrew() bool {
	return r.BrewPath != "" && r.BrewPath != "MISSING" && r.BrewPath != "probe-failed"
}

// runBrewProbe is `fleetctl brew probe` — read nodes.txt, probe each
// host (plus gateway by default), print the markdown table, write JSONL
// events. Exits non-zero if ANY host is missing brew or essentials.
func runBrewProbe(cmd *cobra.Command, _ []string) error {
	logger, err := brewOpenLogger()
	if err != nil {
		return err
	}
	defer logger.Close()
	targets, err := brewResolveTargets()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("brew probe: no targets")
	}

	dialer := newDialer(osh.WithConnectTimeout(brewSSHTimeout))
	reports := make([]brewHostReport, 0, len(targets))
	for _, t := range targets {
		r := brewProbeOne(cmd.Context(), dialer, t, brewEssentialsList(), brewSSHTimeout)
		reports = append(reports, r)
		_ = logger.Action(log.Event{
			Node: r.Host, Phase: "brew",
			Action: "probe", Result: brewLogResult(r),
			StderrTail: brewProbeTail(r),
		})
	}
	if !brewQuiet {
		fmt.Fprint(cmd.OutOrStdout(), brewMarkdownTable(reports))
	}
	if anyHostUnhealthy(reports) {
		cmd.SilenceUsage = true
		return fmt.Errorf("brew probe: %d host(s) need attention", countUnhealthy(reports))
	}
	return nil
}

// runBrewInstall is `fleetctl brew install` — for each host with brew
// + missing essentials (or --force), run `brew install <pkg>...`. Hosts
// without brew get a remediation pointer in the summary.
func runBrewInstall(cmd *cobra.Command, _ []string) error {
	logger, err := brewOpenLogger()
	if err != nil {
		return err
	}
	defer logger.Close()
	targets, err := brewResolveTargets()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("brew install: no targets")
	}
	essentials := brewEssentialsList()
	dialer := newDialer(osh.WithConnectTimeout(brewSSHTimeout))
	reports := make([]brewHostReport, 0, len(targets))
	installFailures := 0

	for _, t := range targets {
		pre := brewProbeOne(cmd.Context(), dialer, t, essentials, brewSSHTimeout)
		_ = logger.Action(log.Event{
			Node: pre.Host, Phase: "brew",
			Action: "probe", Result: brewLogResult(pre),
			StderrTail: brewProbeTail(pre),
		})
		if !pre.HasBrew() {
			reports = append(reports, pre)
			continue
		}
		if len(pre.Missing) == 0 && !brewForce {
			reports = append(reports, pre)
			continue
		}
		toInstall := pre.Missing
		if brewForce && len(toInstall) == 0 {
			toInstall = essentials
		}
		start := time.Now()
		installErr := brewRunInstall(cmd.Context(), dialer, t, pre.BrewPath, toInstall, brewInstallTimeout)
		dur := time.Since(start).Round(time.Second)
		if installErr != nil {
			installFailures++
			_ = logger.Action(log.Event{
				Node: pre.Host, Phase: "brew",
				Action: "install", Result: log.ResultError, DurationMs: dur.Milliseconds(),
				StderrTail: fmt.Sprintf("%v (pkgs=%s)", installErr, strings.Join(toInstall, ",")),
			})
		} else {
			_ = logger.Action(log.Event{
				Node: pre.Host, Phase: "brew",
				Action: "install", Result: log.ResultOK, DurationMs: dur.Milliseconds(),
				StderrTail: fmt.Sprintf("installed=%s duration=%s", strings.Join(toInstall, ","), dur),
			})
		}
		// Re-probe so the summary table reflects post-install state.
		post := brewProbeOne(cmd.Context(), dialer, t, essentials, brewSSHTimeout)
		reports = append(reports, post)
	}

	if !brewQuiet {
		fmt.Fprint(cmd.OutOrStdout(), brewMarkdownTable(reports))
	}
	if installFailures > 0 || anyHostUnhealthy(reports) {
		cmd.SilenceUsage = true
		return fmt.Errorf("brew install: %d install failure(s); %d host(s) still need attention",
			installFailures, countUnhealthy(reports))
	}
	return nil
}

// brewEssentialsList parses the --essentials flag (space-separated).
// Whitespace-only entries are skipped.
func brewEssentialsList() []string {
	raw := strings.Fields(brewEssentials)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// brewResolveTargets returns the SSH targets to probe — the gateway
// first (unless --include-gateway=false) then every worker matching
// the --node filter (or all if no filter).
func brewResolveTargets() ([]brewTarget, error) {
	nodesPath, err := expandHomePath(brewNodesPath)
	if err != nil {
		return nil, err
	}
	reg, err := nodes.Load(nodesPath)
	if err != nil {
		return nil, fmt.Errorf("brew: load nodes: %w", err)
	}
	all := reg.All()
	wanted := map[string]bool{}
	for _, f := range brewFilter {
		wanted[strings.TrimSpace(f)] = true
	}
	out := make([]brewTarget, 0, len(all)+1)
	if brewIncludeGateway && len(brewFilter) == 0 {
		out = append(out, brewTarget{Label: "gateway", Host: shortHostname(), Local: true})
	}
	for _, n := range all {
		if len(wanted) > 0 && !wanted[n.String()] {
			continue
		}
		t, err := osh.ParseTarget(n.String())
		if err != nil {
			return nil, fmt.Errorf("brew: parse %s: %w", n.String(), err)
		}
		out = append(out, brewTarget{Label: n.String(), Host: n.Host, Local: false, SSH: t})
	}
	return out, nil
}

// brewTarget is one host slot for the probe + install loop.
type brewTarget struct {
	Label string     // "gateway" or "user@host"
	Host  string     // short label for the report
	Local bool       // true for the gateway leg (no SSH)
	SSH   osh.Target // populated when Local=false
}

// brewProbeOne runs the probe script against one target and parses the
// stdout into a brewHostReport. The probe script is identical for local
// and remote — for local it's run via sh -c on the gateway.
func brewProbeOne(ctx context.Context, dialer Dialer, t brewTarget, essentials []string, sshTimeout time.Duration) brewHostReport {
	r := brewHostReport{
		Target: t.Label,
		Host:   t.Host,
	}
	probe := brewProbeScript(essentials)
	out, err := brewRunOnTarget(ctx, dialer, t, probe, sshTimeout)
	if err != nil {
		r.BrewPath = "probe-failed"
		r.Err = err.Error()
		return r
	}
	parseBrewProbeOutput(out, &r, essentials)
	return r
}

// brewProbeScript returns the POSIX shell script the probe runs on each
// host. Output format is `key=value` lines that parseBrewProbeOutput
// reads back. Self-contained — no helpers, no Bash-isms.
func brewProbeScript(essentials []string) string {
	var b strings.Builder
	b.WriteString(`arch=$(uname -m 2>/dev/null || echo "?")
brew=""
for p in /opt/homebrew/bin/brew /usr/local/bin/brew; do
  if [ -x "$p" ]; then brew="$p"; break; fi
done
ver=""
if [ -n "$brew" ]; then ver=$("$brew" --version 2>/dev/null | head -1); fi
shellenv=no
if [ -f "$HOME/.local/bin/env" ] && grep -q "brew shellenv" "$HOME/.local/bin/env" 2>/dev/null; then shellenv=yes; fi
printf 'arch=%s\n' "$arch"
printf 'brew=%s\n' "${brew:-MISSING}"
printf 'ver=%s\n' "$ver"
printf 'shellenv=%s\n' "$shellenv"
`)
	for _, e := range essentials {
		bin := binaryFor[e]
		if bin == "" {
			bin = e
		}
		// One line per essential: `present=<formula>` if found, else `missing=<formula>`.
		// We check command -v so a brew-installed binary on PATH counts even
		// if Homebrew is owned by a different user.
		fmt.Fprintf(&b, `if command -v %q >/dev/null 2>&1; then printf 'present=%s\n'; else printf 'missing=%s\n'; fi
`, bin, e, e)
	}
	return b.String()
}

// parseBrewProbeOutput reads the probe script's key=value lines into r.
func parseBrewProbeOutput(out string, r *brewHostReport, essentials []string) {
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
		switch k {
		case "arch":
			r.Arch = v
		case "brew":
			r.BrewPath = v
		case "ver":
			r.BrewVer = v
		case "shellenv":
			r.ShellenvIn = v
		case "present":
			r.Present = append(r.Present, v)
		case "missing":
			r.Missing = append(r.Missing, v)
		}
	}
	sort.Strings(r.Missing)
	sort.Strings(r.Present)
	_ = essentials // reserved for cross-check of unknown formula names; currently advisory
}

// brewRunInstall ssh-invokes `brew install <pkg>...` on the target
// using its absolute brew path so PATH wiring doesn't matter.
func brewRunInstall(ctx context.Context, dialer Dialer, t brewTarget, brewPath string, pkgs []string, timeout time.Duration) error {
	if len(pkgs) == 0 {
		return nil
	}
	if brewPath == "" || brewPath == "MISSING" || brewPath == "probe-failed" {
		return fmt.Errorf("no brew binary on %s", t.Host)
	}
	// `brew install` accepts multiple pkgs at once.
	cmd := fmt.Sprintf("%q install %s", brewPath, strings.Join(quoteAll(pkgs), " "))
	_, err := brewRunOnTarget(ctx, dialer, t, cmd, timeout)
	return err
}

func quoteAll(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// brewRunOnTarget executes script on the target. For Local=true it runs
// via `sh -c` on the gateway (no SSH). For remote it uses the standard
// Dialer + RemoteClient path that delegate.go uses.
func brewRunOnTarget(ctx context.Context, dialer Dialer, t brewTarget, script string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if t.Local {
		return brewRunLocal(runCtx, script)
	}
	client, err := dialer.Dial(runCtx, t.SSH)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", t.Host, err)
	}
	defer client.Close()
	res, err := client.Run(runCtx, script)
	if err != nil {
		return "", fmt.Errorf("ssh run %s: %w", t.Host, err)
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("remote exit %d: %s", res.ExitCode, tailLines(res.Stderr, 1000))
	}
	return res.Stdout, nil
}

// brewRunLocal executes script on the gateway via sh -c. Mocked in tests
// via execCommandContext (the same seam used by backup.go).
func brewRunLocal(ctx context.Context, script string) (string, error) {
	cmd := execCommandContext(ctx, "sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("local sh: %w", err)
	}
	return string(out), nil
}

// --- summary rendering + log helpers ---

func brewLogResult(r brewHostReport) log.Result {
	if r.Err != "" || r.BrewPath == "probe-failed" {
		return log.ResultError
	}
	if !r.HasBrew() || len(r.Missing) > 0 {
		return log.ResultWarn
	}
	return log.ResultOK
}

func brewProbeTail(r brewHostReport) string {
	if r.Err != "" {
		return r.Err
	}
	return fmt.Sprintf("brew=%s ver=%s shellenv=%s missing=%s present=%s",
		r.BrewPath, r.BrewVer, r.ShellenvIn,
		strings.Join(r.Missing, ","), strings.Join(r.Present, ","))
}

func anyHostUnhealthy(reports []brewHostReport) bool {
	return countUnhealthy(reports) > 0
}

func countUnhealthy(reports []brewHostReport) int {
	n := 0
	for _, r := range reports {
		if !r.HasBrew() || len(r.Missing) > 0 || r.Err != "" {
			n++
		}
	}
	return n
}

func brewMarkdownTable(reports []brewHostReport) string {
	var b strings.Builder
	b.WriteString("\n| Host | Arch | Brew | shellenv | Missing | Present |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range reports {
		brewCell := r.BrewPath
		if r.BrewVer != "" {
			brewCell = fmt.Sprintf("%s (%s)", r.BrewPath, r.BrewVer)
		}
		miss := strings.Join(r.Missing, ", ")
		if miss == "" {
			miss = "—"
		}
		pres := strings.Join(r.Present, ", ")
		if pres == "" {
			pres = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			r.Target, r.Arch, brewCell, r.ShellenvIn, miss, pres)
	}
	// Remediation pointers for hosts without brew.
	withoutBrew := []brewHostReport{}
	for _, r := range reports {
		if !r.HasBrew() && r.Err == "" {
			withoutBrew = append(withoutBrew, r)
		}
	}
	if len(withoutBrew) > 0 {
		b.WriteString("\n**Manual admin remediation needed** for hosts above showing `MISSING` — the worker `hermes` user is non-admin and cannot run the Homebrew installer. On each such host, SSH as the admin user and run:\n\n")
		b.WriteString("```\n/bin/bash -c \"$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\"\n```\n\n")
		b.WriteString("Then re-run `fleetctl brew probe` to verify.\n")
	}
	return b.String()
}

func brewOpenLogger() (*log.Logger, error) {
	logPath, err := expandHomePath(brewLogPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("brew: mkdir logs dir: %w", err)
	}
	return openLogger(logPath)
}

func brewSkill() Skill {
	return Skill{
		Name:    "brew",
		Trigger: "/brew",
		Summary: "Probe + install Homebrew essentials across the fleet (supersedes scripts/fleet-brew-baseline.sh).",
		Args: []SkillArg{
			{Name: "probe", Description: "Report Homebrew state per host (no mutations). Non-zero exit when any host is missing brew or essentials."},
			{Name: "install", Description: "Install missing essentials on hosts that already have brew. Hosts without brew get a remediation pointer."},
			{Name: "--essentials \"LIST\"", Description: "Space-separated formula list. Default: jq coreutils shellcheck gh wget ripgrep."},
			{Name: "--node USER@HOST", Description: "Restrict to one worker (repeatable; default all entries in nodes.txt)."},
			{Name: "--include-gateway=false", Description: "Skip the gateway leg (default: gateway included)."},
			{Name: "--ssh-timeout DUR", Description: "Per-host SSH probe timeout. Default 30s."},
			{Name: "--install-timeout DUR", Description: "Per-host install ceiling. Default 5m. (install only)"},
			{Name: "--force", Description: "Re-run install even on hosts with all essentials already present. (install only)"},
			{Name: "--quiet", Description: "Suppress stdout summary; JSONL log unchanged."},
		},
	}
}
