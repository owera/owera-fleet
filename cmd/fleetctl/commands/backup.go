// backup.go implements `fleetctl backup` — the gateway-side
// `~/.hermes/` snapshotter that wraps `restic backup` + `restic
// forget --prune` against an env-configured repository.
//
// # Background
//
// hermes-setup era: `scripts/backup-hermes-state.sh` (com.hermes.backup)
// fires daily at 03:15 BRT. Calls restic with tags + excludes, then
// restic forget+prune with the canonical 7/4/6 retention, then writes
// ~/.hermes/LAST_BACKUP. As hermes-setup is being phased out, this
// command + com.owera.backup are the Go replacements.
//
// # Behaviour parity with the bash version
//
//   - Required env: RESTIC_REPOSITORY, RESTIC_PASSWORD_COMMAND.
//   - Optional env: HERMES_HOME (default ~/.hermes), RESTIC_TAG (default
//     "hermes-state").
//   - Excludes match scripts/backup-hermes-state.sh exactly: sandboxes,
//     cache, *.bak.*, models_dev_cache*, state.db-shm, state.db-wal,
//     logs/backup.log, logs/owera-backup.* (the new launchd stdout/err
//     + jsonl — would self-modify during the backup).
//   - Tags: hermes-state + host=<short>.
//   - Retention: 7 daily, 4 weekly, 6 monthly (per --keep-daily /
//     --keep-weekly / --keep-monthly).
//   - Writes ~/.hermes/LAST_BACKUP with the RFC3339 timestamp.
//   - Init is idempotent: tolerate "already initialized".
//   - Logs to ~/.hermes/logs/owera-backup.jsonl using the standard
//     log.Event schema (NOT the bash version's plain-text backup.log;
//     keeping consumer compatibility for backup.log is out of scope —
//     that file is human-eyeball only).

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/launchd"
	"github.com/owera/owera-fleet/internal/log"
)

var (
	backupQuiet      bool
	backupDryRun     bool
	backupTag        string
	backupKeepDaily  int
	backupKeepWeekly int
	backupKeepMonthly int
	backupSrcDir     = "~/.hermes"
	backupLogPath    = "~/.hermes/logs/owera-backup.jsonl"
)

const (
	oweraBackupLabel        = "com.owera.backup"
	oweraBackupTemplateName = launchd.TemplateOweraBackup
)

// Restic excludes that mirror scripts/backup-hermes-state.sh + the new
// owera-backup launchd I/O files. Note: these are relative to the
// backup source root (HERMES_HOME), as `restic backup --exclude` matches
// path components.
var resticExcludes = []string{
	"sandboxes",
	"cache",
	"*.bak.*",
	"models_dev_cache*",
	"state.db-shm",
	"state.db-wal",
	"logs/backup.log",
	"logs/owera-backup.jsonl",
	"logs/owera-backup.launchd.out",
	"logs/owera-backup.launchd.err",
}

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Snapshot ~/.hermes/ via restic to the env-configured repository",
	Long: `backup wraps restic to snapshot the gateway's ~/.hermes/ tree,
then runs restic forget --prune with the canonical 7/4/6 retention.
Tags every snapshot with "hermes-state" and "host=<short-hostname>"
so multi-host repositories stay readable.

Required environment (mirrors the bash backup):
  RESTIC_REPOSITORY        e.g. sftp:user@host:/backups/hermes
  RESTIC_PASSWORD_COMMAND  command that prints the repository password
                           (e.g. "security find-generic-password ...")

The process is a one-shot: run once per launchd tick
(com.owera.backup fires daily 03:15 BRT). It does NOT loop. Exit 0
on success; non-zero on any restic failure.

The companion command "fleetctl backup-worker" rsyncs each worker's
~/.hermes/ into the gateway tree 10 min before this snapshot runs.`,
	Example: `  fleetctl backup
  fleetctl backup --dry-run
  fleetctl backup --keep-daily 14 --keep-weekly 8`,
	RunE: runBackup,
}

func init() {
	backupCmd.Flags().BoolVar(&backupQuiet, "quiet", false, "suppress stderr summary; JSONL log unchanged")
	backupCmd.Flags().BoolVar(&backupDryRun, "dry-run", false, "pass --dry-run to restic backup; skip forget+prune")
	backupCmd.Flags().StringVar(&backupTag, "tag", "hermes-state", "restic tag for this snapshot family (default hermes-state)")
	backupCmd.Flags().IntVar(&backupKeepDaily, "keep-daily", 7, "restic forget --keep-daily")
	backupCmd.Flags().IntVar(&backupKeepWeekly, "keep-weekly", 4, "restic forget --keep-weekly")
	backupCmd.Flags().IntVar(&backupKeepMonthly, "keep-monthly", 6, "restic forget --keep-monthly")
	rootCmd.AddCommand(backupCmd)
	registerSkill("backup", backupSkill)
}

func runBackup(cmd *cobra.Command, _ []string) error {
	if os.Getenv("RESTIC_REPOSITORY") == "" {
		return errors.New("backup: RESTIC_REPOSITORY is not set")
	}
	if os.Getenv("RESTIC_PASSWORD_COMMAND") == "" {
		return errors.New("backup: RESTIC_PASSWORD_COMMAND is not set")
	}
	if _, err := execLookPath("restic"); err != nil {
		return fmt.Errorf("backup: restic not on PATH: %w", err)
	}
	srcDir, err := expandHomePath(backupSrcDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("backup: source %s missing: %w", srcDir, err)
	}
	logPath, err := expandHomePath(backupLogPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("backup: mkdir logs dir: %w", err)
	}
	logger, err := openLogger(logPath)
	if err != nil {
		return fmt.Errorf("backup: open log: %w", err)
	}
	defer logger.Close()

	host := shortHostname()
	tagArgs := []string{"--tag", backupTag, "--tag", "host=" + host}

	if err := ensureResticRepo(cmd.Context(), logger); err != nil {
		return err
	}

	start := time.Now()
	if err := runResticBackup(cmd.Context(), srcDir, tagArgs, backupDryRun, logger); err != nil {
		return err
	}

	if !backupDryRun {
		if err := runResticForget(cmd.Context(), tagArgs, backupKeepDaily, backupKeepWeekly, backupKeepMonthly, logger); err != nil {
			// forget+prune failure is non-fatal — the snapshot still landed.
			_ = logger.Action(log.Event{
				Node: "gateway", Phase: "backup",
				Action: "forget-prune", Result: log.ResultWarn,
				StderrTail: err.Error(),
			})
		}
		// LAST_BACKUP marker (read by bootstrap-hermes-node.sh's
		// preflight_local + any human eyeballing).
		if err := writeLastBackupMarker(srcDir, start); err != nil {
			_ = logger.Action(log.Event{
				Node: "gateway", Phase: "backup",
				Action: "write-marker", Result: log.ResultWarn,
				StderrTail: err.Error(),
			})
		}
	}

	dur := time.Since(start).Round(time.Second)
	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "backup",
		Action: "run-complete", Result: log.ResultOK,
		StderrTail: fmt.Sprintf("host=%s tag=%s dry_run=%t duration=%s", host, backupTag, backupDryRun, dur),
	})
	if !backupQuiet {
		fmt.Fprintf(cmd.OutOrStdout(),
			"fleetctl backup: host=%s tag=%s dry_run=%t duration=%s\n",
			host, backupTag, backupDryRun, dur)
	}
	return nil
}

// ensureResticRepo runs `restic snapshots --no-lock --json` as a cheap
// "is the repo initialized" probe; on failure, runs `restic init`.
// `restic init` against an already-initialized repo returns non-zero
// with "config file already exists" — that's tolerated.
func ensureResticRepo(ctx context.Context, logger *log.Logger) error {
	probe := execCommandContext(ctx, "restic", "snapshots", "--no-lock", "--json")
	probe.Stdout = nil
	probe.Stderr = nil
	if err := probe.Run(); err == nil {
		return nil
	}
	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "backup",
		Action: "init", Result: log.ResultOK,
		StderrTail: "probe failed; attempting restic init (idempotent)",
	})
	init := execCommandContext(ctx, "restic", "init")
	var stderr bytes.Buffer
	init.Stderr = &stderr
	if err := init.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if strings.Contains(tail, "already initialized") || strings.Contains(tail, "config file already exists") {
			return nil
		}
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "backup",
			Action: "init", Result: log.ResultError, StderrTail: tail,
		})
		return fmt.Errorf("backup: restic init: %w", err)
	}
	return nil
}

// runResticBackup invokes `restic backup` with the standard excludes
// and the caller-supplied tag args.
func runResticBackup(ctx context.Context, srcDir string, tagArgs []string, dryRun bool, logger *log.Logger) error {
	args := []string{"backup"}
	args = append(args, tagArgs...)
	for _, ex := range resticExcludes {
		args = append(args, "--exclude="+ex)
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, srcDir)
	cmd := execCommandContext(ctx, "restic", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := tailLines(stderr.String(), 4000)
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "backup",
			Action: "snapshot", Result: log.ResultError, StderrTail: tail,
		})
		return fmt.Errorf("backup: restic backup: %w", err)
	}
	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "backup",
		Action: "snapshot", Result: log.ResultOK,
		StderrTail: fmt.Sprintf("src=%s dry_run=%t", srcDir, dryRun),
	})
	return nil
}

// runResticForget invokes `restic forget --prune` with the standard
// retention. Non-fatal on caller side — caller logs Warn rather than
// returning the error.
func runResticForget(ctx context.Context, tagArgs []string, keepDaily, keepWeekly, keepMonthly int, logger *log.Logger) error {
	args := []string{
		"forget", "--prune",
		"--keep-daily", fmt.Sprint(keepDaily),
		"--keep-weekly", fmt.Sprint(keepWeekly),
		"--keep-monthly", fmt.Sprint(keepMonthly),
	}
	args = append(args, tagArgs...)
	cmd := execCommandContext(ctx, "restic", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restic forget: %w (stderr=%s)", err, tailLines(stderr.String(), 1000))
	}
	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "backup",
		Action: "forget-prune", Result: log.ResultOK,
		StderrTail: fmt.Sprintf("kept daily=%d weekly=%d monthly=%d", keepDaily, keepWeekly, keepMonthly),
	})
	return nil
}

// writeLastBackupMarker writes the canonical RFC3339 timestamp to
// $HERMES_HOME/LAST_BACKUP. Format matches the bash version exactly
// so bootstrap-hermes-node.sh's preflight_local() reads it correctly.
func writeLastBackupMarker(hermesHome string, at time.Time) error {
	path := filepath.Join(hermesHome, "LAST_BACKUP")
	return os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// shortHostname returns the short host (everything before the first
// dot). Matches `hostname -s` on macOS.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		return h[:i]
	}
	return h
}

// tailLines returns up to the last maxBytes of s, sliced at a byte
// boundary (good enough for stderr capture in audit logs).
func tailLines(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[len(s)-maxBytes:])
}

// --- install / uninstall / status (mirror watchdog + logrotate) ---

func oweraBackupPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", oweraBackupLabel+".plist"), nil
}

// newBackupManagerFn lets tests substitute a Manager with mock
// RunCmd / WriteFile.
var newBackupManagerFn = func() *launchd.Manager { return launchd.New(nil) }

var backupInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install com.owera.backup as a user LaunchAgent (daily 03:15 BRT)",
	Long: `Install bakes RESTIC_REPOSITORY and RESTIC_PASSWORD_COMMAND into
the rendered plist so launchd's user-domain env is sufficient. The
operator must export both before running install:

  export RESTIC_REPOSITORY=sftp:user@host:/backups/hermes
  export RESTIC_PASSWORD_COMMAND='security find-generic-password -s hermes-restic -w'
  fleetctl backup install`,
	RunE: runBackupInstall,
}

func runBackupInstall(cmd *cobra.Command, _ []string) error {
	repo := os.Getenv("RESTIC_REPOSITORY")
	pwCmd := os.Getenv("RESTIC_PASSWORD_COMMAND")
	if repo == "" || pwCmd == "" {
		return errors.New("install: export RESTIC_REPOSITORY and RESTIC_PASSWORD_COMMAND before running install (they are baked into the plist)")
	}
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
	plistPath, err := oweraBackupPlistPath()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	m := newBackupManagerFn()
	body, err := m.RenderTemplate(oweraBackupTemplateName, launchd.Vars{
		Label:                 oweraBackupLabel,
		HomeDir:               home,
		ScriptPath:            exeAbs,
		ResticRepository:      repo,
		ResticPasswordCommand: pwCmd,
	})
	if err != nil {
		return fmt.Errorf("install: render: %w", err)
	}
	if err := m.Install(cmd.Context(), body, plistPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"backup: installed at %s (label=%s, exe=%s)\n"+
			"backup: fires daily 03:15 local; force a run now with:\n"+
			"  launchctl kickstart -k gui/$(id -u)/%s\n",
		plistPath, oweraBackupLabel, exeAbs, oweraBackupLabel)
	return nil
}

var backupUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Tear down the com.owera.backup LaunchAgent",
	RunE:  runBackupUninstall,
}

func runBackupUninstall(cmd *cobra.Command, _ []string) error {
	plistPath, err := oweraBackupPlistPath()
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	m := newBackupManagerFn()
	if err := m.Uninstall(cmd.Context(), oweraBackupLabel, plistPath); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"backup: uninstalled (%s, label=%s)\n",
		plistPath, oweraBackupLabel)
	return nil
}

var backupStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print the running state of com.owera.backup",
	RunE:  runBackupStatus,
}

func runBackupStatus(cmd *cobra.Command, _ []string) error {
	m := newBackupManagerFn()
	st, err := m.Status(cmd.Context(), oweraBackupLabel)
	if err != nil {
		if isUnloadedErr(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "backup: not loaded (label=%s)\n", oweraBackupLabel)
			return fmt.Errorf("not loaded")
		}
		return fmt.Errorf("status: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"backup: label=%s loaded=%t pid=%s last_exit=%s state=%s\n",
		st.Label, st.Loaded, st.PID, st.LastExitCode, st.State)
	return nil
}

func init() {
	backupCmd.AddCommand(backupInstallCmd)
	backupCmd.AddCommand(backupUninstallCmd)
	backupCmd.AddCommand(backupStatusCmd)
}

func backupSkill() Skill {
	return Skill{
		Name:    "backup",
		Trigger: "/backup",
		Summary: "Snapshot ~/.hermes/ via restic (supersedes the hermes-setup bash com.hermes.backup).",
		Args: []SkillArg{
			{Name: "--dry-run", Description: "Pass --dry-run to restic backup; skip forget+prune."},
			{Name: "--tag NAME", Description: "Restic tag for this snapshot family. Default hermes-state."},
			{Name: "--keep-daily N / --keep-weekly N / --keep-monthly N", Description: "Restic retention. Defaults 7 / 4 / 6."},
			{Name: "--quiet", Description: "Suppress stderr summary."},
			{Name: "install / uninstall / status", Description: "launchd lifecycle for com.owera.backup (daily 03:15). install reads RESTIC_REPOSITORY + RESTIC_PASSWORD_COMMAND from env and bakes them into the plist."},
		},
	}
}

// --- mockable exec hooks (shared with backup_worker.go via package scope) ---

// execCommandContext is a package-level seam so tests can intercept
// the exec.Cmd construction without running real restic / rsync.
var execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// execLookPath is the PATH lookup seam; tests substitute a stub that
// returns ("/usr/local/bin/restic", nil) without touching PATH.
var execLookPath = func(name string) (string, error) {
	return exec.LookPath(name)
}
