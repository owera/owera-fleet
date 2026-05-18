// healthdiff.go implements `fleetctl health-diff` — the gateway-side
// snapshot differ that compares two fleet-health-snapshot markdown
// reports and produces a structured change list.
//
// # Background
//
// hermes-setup era: scripts/fleet-health-diff.sh, a 250-line bash
// script with two awk programs (one extract, one diff). Ran on demand
// from the gateway operator's shell, no scheduling.
//
// owera-fleet era: this command supersedes the bash. Parsing rules are
// preserved one-for-one in internal/healthdiff (the bash awk's quirks
// are documented inline there); the new value is:
//
//   - Severity classification (info / warn / crit) on every change
//     record, so CI can gate on warn-or-above.
//   - Exit codes that map to severity (0 = no diff, 1 = info-only,
//     2 = warn-or-crit). The bash version always returned 0.
//   - A first-class JSON format with a stable schema.
//
// The command has no install/uninstall lifecycle — it's a one-shot
// invocation, run by the operator (or by CI). The bash original
// neither installed a LaunchAgent nor needed one; the Go port keeps
// that property.

package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/healthdiff"
	"github.com/owera/owera-fleet/internal/log"
)

var (
	healthDiffFromPath    string
	healthDiffToPath      string
	healthDiffFormat      string
	healthDiffReportsDir  = "~/.hermes/reports"
	healthDiffLogPath     = "~/.hermes/logs/fleet-health-diff.jsonl"
	healthDiffSuppressLog bool
)

var healthDiffCmd = &cobra.Command{
	Use:   "health-diff [fromReport toReport]",
	Short: "Diff two fleet-health-snapshot reports; classify changes by severity",
	Long: `health-diff compares two fleet-health-snapshot markdown reports and
emits a structured diff. Two invocation styles:

  fleetctl health-diff                      # diff the two newest in ~/.hermes/reports
  fleetctl health-diff a.md b.md            # explicit positional args
  fleetctl health-diff --from a.md --to b.md
  fleetctl health-diff --format json a.md b.md

Each change record carries a severity label (info / warn / crit).
crit  → host disappeared, probe started failing
warn  → disk creep, mem dropped, heartbeat going stale, brew drift
info  → version bumps, pinned-version touches, hostname/kernel changes

Exit codes:
  0  no changes
  1  info-only changes
  2  any warn or crit change

Output formats:
  markdown (default)  the per-host bullet list operators read
  json                machine-parseable, stable schema (host, kind,
                      field, old, new, severity)

A JSONL action event is appended to ~/.hermes/logs/fleet-health-diff.jsonl
on every run, mirroring the bash port's audit trail.`,
	Example: `  fleetctl health-diff
  fleetctl health-diff --format json
  fleetctl health-diff a.md b.md
  fleetctl health-diff --from old.md --to new.md --format json`,
	RunE: runHealthDiff,
	Args: cobra.MaximumNArgs(2),
}

func init() {
	healthDiffCmd.Flags().StringVar(&healthDiffFromPath, "from", "", "first (older) snapshot path; defaults to second-newest in ~/.hermes/reports")
	healthDiffCmd.Flags().StringVar(&healthDiffToPath, "to", "", "second (newer) snapshot path; defaults to newest in ~/.hermes/reports")
	healthDiffCmd.Flags().StringVar(&healthDiffFormat, "format", "markdown", "output format: markdown or json")
	healthDiffCmd.Flags().BoolVar(&healthDiffSuppressLog, "no-log", false, "skip appending to ~/.hermes/logs/fleet-health-diff.jsonl")
	rootCmd.AddCommand(healthDiffCmd)
	registerSkill("health-diff", healthDiffSkill)
}

// healthDiffExit lets tests intercept os.Exit so they don't kill the
// test binary. Production wraps os.Exit directly.
var healthDiffExit = os.Exit

func runHealthDiff(cmd *cobra.Command, args []string) error {
	// Positional arguments override --from/--to. This matches operator
	// muscle memory ("diff oldfile newfile") and stays compatible with
	// the bash port's flag-only contract.
	switch len(args) {
	case 2:
		healthDiffFromPath, healthDiffToPath = args[0], args[1]
	case 1:
		return fmt.Errorf("health-diff: need two positional args (from to), got 1")
	}

	from, to, err := resolveHealthDiffPaths(healthDiffFromPath, healthDiffToPath)
	if err != nil {
		return err
	}
	if err := assertFile(from); err != nil {
		return fmt.Errorf("health-diff: --from: %w", err)
	}
	if err := assertFile(to); err != nil {
		return fmt.Errorf("health-diff: --to: %w", err)
	}

	fromBody, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("health-diff: read from: %w", err)
	}
	toBody, err := os.ReadFile(to)
	if err != nil {
		return fmt.Errorf("health-diff: read to: %w", err)
	}

	fromSnap, err := healthdiff.Parse(string(fromBody))
	if err != nil {
		return fmt.Errorf("health-diff: parse from: %w", err)
	}
	toSnap, err := healthdiff.Parse(string(toBody))
	if err != nil {
		return fmt.Errorf("health-diff: parse to: %w", err)
	}

	report := healthdiff.Diff(fromSnap, toSnap)
	report.FromName = filepath.Base(from)
	report.ToName = filepath.Base(to)

	if err := emitHealthDiff(cmd, report, healthDiffFormat); err != nil {
		return err
	}

	if !healthDiffSuppressLog {
		// Best-effort logging — we don't fail the command if the log
		// path can't be opened, matching the bash port's behaviour.
		_ = appendHealthDiffLog(report)
	}

	// Map severity to exit code so CI can gate on warn-or-above.
	switch report.MaxSeverity() {
	case healthdiff.SeverityCrit, healthdiff.SeverityWarn:
		healthDiffExit(2)
	case healthdiff.SeverityInfo:
		healthDiffExit(1)
	}
	// MaxSeverity returns "" when there are zero changes; fall through
	// to a clean exit 0 in that case.
	return nil
}

// resolveHealthDiffPaths fills in the default "two newest snapshots"
// case. When both args are empty, scan ~/.hermes/reports for
// fleet-snapshot-*.md and pick the two lexicographically-largest
// (matches the bash port's `ls -1 | sort | tail -2`, which works
// because the timestamps are sort-friendly).
func resolveHealthDiffPaths(fromArg, toArg string) (string, string, error) {
	if fromArg != "" && toArg != "" {
		return fromArg, toArg, nil
	}
	if fromArg != "" || toArg != "" {
		return "", "", errors.New("health-diff: --from and --to must be specified together (or omit both for the default)")
	}
	dir, err := expandHomePath(healthDiffReportsDir)
	if err != nil {
		return "", "", fmt.Errorf("health-diff: resolve reports dir: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", fmt.Errorf("health-diff: no reports dir at %s (run `fleetctl health --all > %s/fleet-snapshot-<ts>.md` first)", dir, dir)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("health-diff: %s is not a directory", dir)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "fleet-snapshot-*.md"))
	if err != nil {
		return "", "", fmt.Errorf("health-diff: scan reports: %w", err)
	}
	if len(matches) < 2 {
		return "", "", fmt.Errorf("health-diff: need at least 2 snapshots in %s; found %d", dir, len(matches))
	}
	sort.Strings(matches)
	return matches[len(matches)-2], matches[len(matches)-1], nil
}

// assertFile returns an error if path is not an existing regular file.
// Operators sometimes typo --from / --to with a stale path; this surfaces
// it before we waste cycles on a half-parsed report.
func assertFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a snapshot file", path)
	}
	return nil
}

func emitHealthDiff(cmd *cobra.Command, r healthdiff.Report, format string) error {
	w := cmd.OutOrStdout()
	switch strings.ToLower(format) {
	case "", "markdown", "md":
		_, err := w.Write([]byte(healthdiff.RenderMarkdown(r)))
		return err
	case "json":
		body, err := healthdiff.RenderJSON(r)
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	default:
		return fmt.Errorf("health-diff: unknown --format %q (want markdown or json)", format)
	}
}

func appendHealthDiffLog(r healthdiff.Report) error {
	resolved, err := expandHomePath(healthDiffLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(resolved)
	if err != nil {
		return err
	}
	defer logger.Close()
	tail := fmt.Sprintf("from=%s to=%s changes=%d severity=%s",
		r.FromName, r.ToName, r.TotalChanges, r.MaxSeverity())
	return logger.Action(log.Event{
		Node:       "gateway",
		Phase:      "health-diff",
		Action:     "run-complete",
		Result:     resultForSeverity(r.MaxSeverity()),
		StderrTail: tail,
	})
}

func resultForSeverity(s healthdiff.Severity) log.Result {
	switch s {
	case healthdiff.SeverityCrit:
		return log.ResultError
	case healthdiff.SeverityWarn:
		return log.ResultWarn
	case healthdiff.SeverityInfo:
		return log.ResultOK
	}
	// Empty severity (no changes) → no-change so audit tooling can tell
	// quiet runs from genuinely-OK ones.
	return log.ResultNoChange
}

func healthDiffSkill() Skill {
	return Skill{
		Name:    "health-diff",
		Trigger: "/health-diff",
		Summary: "Diff two fleet-health-snapshot reports; classify each change by severity (info/warn/crit) and exit non-zero on warn-or-above. Supersedes the hermes-setup bash differ.",
		Args: []SkillArg{
			{Name: "<fromReport> <toReport>", Description: "Positional snapshot paths. Override --from/--to when given."},
			{Name: "--from PATH", Description: "Older snapshot. Pair with --to."},
			{Name: "--to PATH", Description: "Newer snapshot. Pair with --from."},
			{Name: "--format FMT", Description: "Output format: markdown (default) or json. markdown is the human bullet list; json is machine-parseable."},
			{Name: "--no-log", Description: "Skip appending to ~/.hermes/logs/fleet-health-diff.jsonl."},
		},
		Examples: []SkillExample{
			{Description: "Diff the two newest snapshots in ~/.hermes/reports/", Command: "fleetctl health-diff"},
			{Description: "Diff two specific files", Command: "fleetctl health-diff a.md b.md"},
			{Description: "CI gate (exits non-zero on warn-or-above)", Command: "fleetctl health-diff --format json"},
		},
	}
}
