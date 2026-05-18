// logrotate.go implements `fleetctl logrotate` — the gateway-side
// log rotator that gzip-rotates `~/.hermes/logs/*.jsonl` files above a
// size threshold and prunes old rotations.
//
// # Background
//
// hermes-setup era: `scripts/rotate-hermes-logs.sh` (com.hermes.logrotate)
// fires daily at 03:30 BRT. cp+gzip the source to <name>.<ts>.gz, then
// `:> file` to truncate in place — preserves any open append fd's at
// the cost of (rarely) one log line written during the cp→truncate
// window.
//
// owera-fleet era: this command + com.owera.logrotate supersede the
// bash. Behaviour-equivalent: same threshold default (1 MiB), same
// keep default (5 rotations per log), same scope (*.jsonl only, never
// Hermes-owned *.log files), same atomic-rename truncate strategy.
//
// # Scope
//
// Only *.jsonl files are rotated. The Hermes-owned .log files
// (agent.log, backup.log, errors.log, update.log) grow slowly and
// truncating them would be invasive — they're left alone, same as
// the bash version. The rotator's own log file (whatever path
// LOGPATH points to) is also skipped to avoid self-rotation.

package commands

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/launchd"
	"github.com/owera/owera-fleet/internal/log"
)

var (
	logrotateThresholdBytes int64
	logrotateKeep           int
	logrotateDryRun         bool
	logrotateQuiet          bool
	logrotateLogsDir        = "~/.hermes/logs"
	logrotateLogPath        = "~/.hermes/logs/owera-logrotate.jsonl"
)

const (
	oweraLogrotateLabel        = "com.owera.logrotate"
	oweraLogrotateTemplateName = launchd.TemplateOweraLogrotate
)

var logrotateCmd = &cobra.Command{
	Use:   "logrotate",
	Short: "Gzip-rotate ~/.hermes/logs/*.jsonl files above --threshold-bytes",
	Long: `logrotate scans ~/.hermes/logs for *.jsonl files. Any file at
or above --threshold-bytes is gzip-copied to <name>.<utc-ts>.gz and
then truncated in place (preserving open append fd's of concurrent
writers). Per-log rotations beyond --keep are pruned oldest-first.

Defaults match the legacy bash rotator:
  --threshold-bytes  1048576   (1 MiB; env: HERMES_LOG_ROTATE_THRESHOLD)
  --keep             5         (env: HERMES_LOG_ROTATE_KEEP)
  --dry-run          false     (plan only; touch nothing)
  --quiet            false     (suppress stderr; JSONL log unchanged)

The rotator is a one-shot: run once per launchd tick (com.owera.logrotate
fires daily 03:30 BRT). It does NOT loop. Use --dry-run before changing
threshold/keep in production.

Hermes-owned *.log files (agent.log, backup.log, errors.log, update.log)
are intentionally NOT rotated — truncating them is invasive and they
grow slowly.`,
	Example: `  fleetctl logrotate
  fleetctl logrotate --threshold-bytes 1024 --dry-run
  fleetctl logrotate --keep 10 --quiet`,
	RunE: runLogrotate,
}

func init() {
	logrotateCmd.Flags().Int64Var(&logrotateThresholdBytes, "threshold-bytes", 0, "rotate files at or above this size (bytes); 0 means honour HERMES_LOG_ROTATE_THRESHOLD or default 1 MiB")
	logrotateCmd.Flags().IntVar(&logrotateKeep, "keep", 0, "keep this many newest rotations per log; 0 means honour HERMES_LOG_ROTATE_KEEP or default 5")
	logrotateCmd.Flags().BoolVar(&logrotateDryRun, "dry-run", false, "plan only; do not rotate or prune")
	logrotateCmd.Flags().BoolVar(&logrotateQuiet, "quiet", false, "suppress stderr summary; JSONL log is unchanged")
	rootCmd.AddCommand(logrotateCmd)
	registerSkill("logrotate", logrotateSkill)
}

func runLogrotate(cmd *cobra.Command, _ []string) error {
	threshold := resolveThresholdBytes()
	keep := resolveKeep()
	if threshold < 1 {
		return fmt.Errorf("logrotate: threshold-bytes must be >= 1 (got %d)", threshold)
	}
	if keep < 1 {
		return fmt.Errorf("logrotate: keep must be >= 1 (got %d)", keep)
	}
	logsDir, err := expandHomePath(logrotateLogsDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("logrotate: mkdir %s: %w", logsDir, err)
	}
	logPath, err := expandHomePath(logrotateLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(logPath)
	if err != nil {
		return fmt.Errorf("logrotate: open log: %w", err)
	}
	defer logger.Close()

	summary, err := rotateAll(cmd.Context(), logsDir, logPath, threshold, keep, logrotateDryRun, logger)
	if err != nil {
		return err
	}
	if !logrotateQuiet {
		fmt.Fprintf(cmd.OutOrStdout(),
			"fleetctl logrotate: scanned=%d rotated=%d skipped=%d pruned=%d errors=%d dry_run=%t threshold=%d keep=%d\n",
			summary.Scanned, summary.Rotated, summary.Skipped, summary.Pruned, summary.Errors,
			logrotateDryRun, threshold, keep)
	}
	return nil
}

func resolveThresholdBytes() int64 {
	if logrotateThresholdBytes > 0 {
		return logrotateThresholdBytes
	}
	if v := os.Getenv("HERMES_LOG_ROTATE_THRESHOLD"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 1 << 20 // 1 MiB
}

func resolveKeep() int {
	if logrotateKeep > 0 {
		return logrotateKeep
	}
	if v := os.Getenv("HERMES_LOG_ROTATE_KEEP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 5
}

// rotationSummary counts the per-run outcomes for the stderr summary
// line + the run-complete log entry.
type rotationSummary struct {
	Scanned int
	Rotated int
	Skipped int
	Pruned  int
	Errors  int
}

// rotateAll iterates *.jsonl files in dir and rotates each one over
// threshold. selfLogPath is excluded from rotation so the rotator can
// freely write to it during its own run. Returns the summary; any
// per-file error is counted in Errors but never aborts the loop.
func rotateAll(ctx context.Context, dir, selfLogPath string, threshold int64, keep int, dryRun bool, logger *log.Logger) (rotationSummary, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "logrotate",
			Action: "read-dir", Result: log.ResultError, StderrTail: err.Error(),
		})
		return rotationSummary{}, fmt.Errorf("logrotate: read %s: %w", dir, err)
	}
	var summary rotationSummary
	selfBase := filepath.Base(selfLogPath)
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".jsonl") || name == selfBase {
			continue
		}
		summary.Scanned++
		path := filepath.Join(dir, name)
		info, err := ent.Info()
		if err != nil {
			summary.Errors++
			_ = logger.Action(log.Event{
				Node: name, Phase: "logrotate",
				Action: "stat", Result: log.ResultError, StderrTail: err.Error(),
			})
			continue
		}
		if info.Size() < threshold {
			summary.Skipped++
			_ = logger.Action(log.Event{
				Node: name, Phase: "logrotate",
				Action: "skip", Result: log.ResultOK,
				StderrTail: fmt.Sprintf("size=%d threshold=%d", info.Size(), threshold),
			})
			continue
		}
		if dryRun {
			summary.Rotated++ // counted as "would rotate" in dry-run
			_ = logger.Action(log.Event{
				Node: name, Phase: "logrotate",
				Action: "rotate", Result: log.ResultOK,
				StderrTail: fmt.Sprintf("dry-run size=%d", info.Size()),
			})
			continue
		}
		rotatedPath, err := rotateOne(path, time.Now().UTC())
		if err != nil {
			summary.Errors++
			_ = logger.Action(log.Event{
				Node: name, Phase: "logrotate",
				Action: "rotate", Result: log.ResultError, StderrTail: err.Error(),
			})
			continue
		}
		summary.Rotated++
		_ = logger.Action(log.Event{
			Node: name, Phase: "logrotate",
			Action: "rotate", Result: log.ResultOK,
			StderrTail: fmt.Sprintf("size=%d -> %s", info.Size(), filepath.Base(rotatedPath)),
		})
		pruned, err := pruneRotations(path, keep)
		summary.Pruned += pruned
		if err != nil {
			summary.Errors++
			_ = logger.Action(log.Event{
				Node: name, Phase: "logrotate",
				Action: "prune", Result: log.ResultError, StderrTail: err.Error(),
			})
		}
	}
	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "logrotate",
		Action: "run-complete", Result: log.ResultOK,
		StderrTail: fmt.Sprintf("scanned=%d rotated=%d skipped=%d pruned=%d errors=%d threshold=%d keep=%d dry_run=%t",
			summary.Scanned, summary.Rotated, summary.Skipped, summary.Pruned, summary.Errors, threshold, keep, dryRun),
	})
	_ = ctx // reserved for future cancellation
	return summary, nil
}

// rotateOne gzips path to a temp file, renames to the final
// `<path>.<ts>.gz`, and truncates path in place. Returns the final
// rotated path on success. Uses an atomic rename so a concurrent
// listing never sees a half-written `.gz` under the final name.
func rotateOne(path string, now time.Time) (string, error) {
	ts := now.Format("20060102T150405Z")
	finalPath := path + "." + ts + ".gz"
	tmpPath := path + ".rotating." + strconv.Itoa(os.Getpid())
	src, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open src: %w", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	gz := gzip.NewWriter(dst)
	if _, err := io.Copy(gz, src); err != nil {
		_ = gz.Close()
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("gzip copy: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("gzip close: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("tmp close: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename: %w", err)
	}
	// Truncate in place so concurrent append-mode writers keep their
	// fd's. os.Truncate is atomic at the inode level.
	if err := os.Truncate(path, 0); err != nil {
		return finalPath, fmt.Errorf("truncate src: %w", err)
	}
	return finalPath, nil
}

// pruneRotations removes the oldest .gz rotations of basePath beyond
// `keep`. Rotations are matched by glob `basePath.*.gz` and sorted by
// filename (which is timestamp-sortable per rotateOne's ts format).
// Returns the count of files removed.
func pruneRotations(basePath string, keep int) (int, error) {
	matches, err := filepath.Glob(basePath + ".*.gz")
	if err != nil {
		return 0, err
	}
	if len(matches) <= keep {
		return 0, nil
	}
	// Sort by name: lexicographic order on the YYYYMMDDTHHMMSSZ stamp
	// is the same as chronological order, oldest first.
	sort.Strings(matches)
	toDelete := matches[:len(matches)-keep]
	var pruned int
	var firstErr error
	for _, p := range toDelete {
		if err := os.Remove(p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		pruned++
	}
	return pruned, firstErr
}

// --- install / uninstall / status (mirror watchdog pattern) ---

func oweraLogrotatePlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", oweraLogrotateLabel+".plist"), nil
}

// newLogrotateManagerFn lets tests substitute a Manager with mock
// RunCmd / WriteFile.
var newLogrotateManagerFn = func() *launchd.Manager { return launchd.New(nil) }

var logrotateInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install com.owera.logrotate as a user LaunchAgent (daily 03:30 BRT)",
	RunE:  runLogrotateInstall,
}

func runLogrotateInstall(cmd *cobra.Command, _ []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("install: home dir: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("install: os.Executable: %w", err)
	}
	exeAbs, err := filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("install: abs: %w", err)
	}
	plistPath, err := oweraLogrotatePlistPath()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	m := newLogrotateManagerFn()
	body, err := m.RenderTemplate(oweraLogrotateTemplateName, launchd.Vars{
		Label:      oweraLogrotateLabel,
		HomeDir:    home,
		ScriptPath: exeAbs,
	})
	if err != nil {
		return fmt.Errorf("install: render: %w", err)
	}
	if err := m.Install(cmd.Context(), body, plistPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"logrotate: installed at %s (label=%s, exe=%s)\n"+
			"logrotate: fires daily 03:30 local; threshold/keep configurable via env:\n"+
			"  launchctl setenv HERMES_LOG_ROTATE_THRESHOLD 2097152   # 2 MiB\n"+
			"  launchctl setenv HERMES_LOG_ROTATE_KEEP 10\n"+
			"  launchctl kickstart -k gui/$(id -u)/%s   # force one-shot now\n",
		plistPath, oweraLogrotateLabel, exeAbs, oweraLogrotateLabel)
	return nil
}

var logrotateUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Tear down the com.owera.logrotate LaunchAgent",
	RunE:  runLogrotateUninstall,
}

func runLogrotateUninstall(cmd *cobra.Command, _ []string) error {
	plistPath, err := oweraLogrotatePlistPath()
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	m := newLogrotateManagerFn()
	if err := m.Uninstall(cmd.Context(), oweraLogrotateLabel, plistPath); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"logrotate: uninstalled (%s, label=%s)\n",
		plistPath, oweraLogrotateLabel)
	return nil
}

var logrotateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print the running state of com.owera.logrotate",
	RunE:  runLogrotateStatus,
}

func runLogrotateStatus(cmd *cobra.Command, _ []string) error {
	m := newLogrotateManagerFn()
	st, err := m.Status(cmd.Context(), oweraLogrotateLabel)
	if err != nil {
		if isUnloadedErr(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "logrotate: not loaded (label=%s)\n", oweraLogrotateLabel)
			return fmt.Errorf("not loaded")
		}
		return fmt.Errorf("status: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"logrotate: label=%s loaded=%t pid=%s last_exit=%s state=%s\n",
		st.Label, st.Loaded, st.PID, st.LastExitCode, st.State)
	return nil
}

// isUnloadedErr wraps the launchd.ErrUnloaded check so we don't need
// to import errors in this file just for one errors.Is call. Same
// behaviour as the watchdog status handler.
func isUnloadedErr(err error) bool {
	return err != nil && err.Error() == launchd.ErrUnloaded.Error()
}

func init() {
	logrotateCmd.AddCommand(logrotateInstallCmd)
	logrotateCmd.AddCommand(logrotateUninstallCmd)
	logrotateCmd.AddCommand(logrotateStatusCmd)
}

func logrotateSkill() Skill {
	return Skill{
		Name:    "logrotate",
		Trigger: "/logrotate",
		Summary: "Gzip-rotate ~/.hermes/logs/*.jsonl files above the size threshold; supersedes the hermes-setup bash rotator.",
		Args: []SkillArg{
			{Name: "--threshold-bytes N", Description: "Rotate files at or above this size in bytes. Default 1 MiB (env: HERMES_LOG_ROTATE_THRESHOLD)."},
			{Name: "--keep N", Description: "Keep this many newest rotations per log. Default 5 (env: HERMES_LOG_ROTATE_KEEP)."},
			{Name: "--dry-run", Description: "Plan only; touch nothing."},
			{Name: "--quiet", Description: "Suppress stderr summary; JSONL log unchanged."},
			{Name: "install / uninstall / status", Description: "launchd lifecycle for com.owera.logrotate (daily 03:30)."},
		},
	}
}
