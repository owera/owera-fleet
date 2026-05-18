// backup_worker.go implements `fleetctl backup-worker` — pulls each
// worker's ~/.hermes/ into ~/.hermes/worker-backups/<host>/ on the
// gateway via rsync. The companion `fleetctl backup` snapshot picks
// up worker-backups/ as part of its own restic run 10 min later.
//
// # Background
//
// hermes-setup era: `scripts/backup-worker-state.sh`
// (com.hermes.backup-worker) fires daily at 03:05 BRT. Best-effort:
// per-worker failures do not halt the loop. As hermes-setup is being
// phased out, this command + com.owera.backup-worker are the Go
// replacements.
//
// # Behaviour parity
//
//   - Reads ~/.hermes/nodes.txt for the worker list (same file the
//     watchdog + heartbeats-bridge consume).
//   - Per-worker rsync with the same excludes as the bash version:
//     sandboxes, cache, *.bak.*, models_dev_cache*, state.db-shm,
//     state.db-wal, logs, hermes-agent, node, bin.
//   - rsync args: -az --delete --partial --inplace -e ssh-with-timeout.
//   - Per-worker .LAST_PULL marker written on success.
//   - Per-worker failures logged + counted; loop continues.
//   - Best-effort: exit non-zero if any worker failed but partial
//     pulls still land.
//
// We deliberately do NOT inherit the bash version's ssh-drains-stdin
// risk (PR #2 in hermes-setup fixed the watchdog version). Go's
// exec.Command does not pipe parent stdin to the child unless we set
// Stdin explicitly, and we never do.

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/launchd"
	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
)

var (
	backupWorkerQuiet      bool
	backupWorkerDryRun     bool
	backupWorkerSSHTimeout time.Duration
	backupWorkerBwLimit    int
	backupWorkerFilter     []string
	backupWorkerNodesPath  = "~/.hermes/nodes.txt"
	backupWorkerDestRoot   = "~/.hermes/worker-backups"
	backupWorkerLogPath    = "~/.hermes/logs/owera-backup-worker.jsonl"
)

const (
	oweraBackupWorkerLabel        = "com.owera.backup-worker"
	oweraBackupWorkerTemplateName = launchd.TemplateOweraBackupWorker
)

// rsyncExcludes mirror scripts/backup-worker-state.sh exactly.
var rsyncExcludes = []string{
	"sandboxes",
	"cache",
	"*.bak.*",
	"models_dev_cache*",
	"state.db-shm",
	"state.db-wal",
	"logs",
	"hermes-agent",
	"node",
	"bin",
}

var backupWorkerCmd = &cobra.Command{
	Use:   "backup-worker",
	Short: "rsync each worker's ~/.hermes/ into ~/.hermes/worker-backups/<host>/",
	Long: `backup-worker iterates ~/.hermes/nodes.txt and rsyncs each
worker's ~/.hermes/ (minus regeneratable trees: cache, sandboxes,
hermes-agent, node, bin, logs) into the gateway's
~/.hermes/worker-backups/<host>/.

The companion command "fleetctl backup" picks this whole tree up as
part of its restic snapshot 10 min later (03:15 BRT) so worker state
flows through the same off-host backup as gateway state — no separate
restic credentials per worker.

Best-effort: per-worker failures do NOT halt the loop. Exit code
reflects whether ANY worker failed so launchd's StandardErrorPath
captures it for the operator.`,
	Example: `  fleetctl backup-worker
  fleetctl backup-worker --dry-run
  fleetctl backup-worker --node hermes@claw1.local
  fleetctl backup-worker --bwlimit 2048   # KB/s`,
	RunE: runBackupWorker,
}

func init() {
	backupWorkerCmd.Flags().BoolVar(&backupWorkerQuiet, "quiet", false, "suppress stderr summary; JSONL log unchanged")
	backupWorkerCmd.Flags().BoolVar(&backupWorkerDryRun, "dry-run", false, "pass --dry-run -i to rsync; touch no destination files")
	backupWorkerCmd.Flags().DurationVar(&backupWorkerSSHTimeout, "ssh-timeout", 15*time.Second, "rsync's ssh -o ConnectTimeout value")
	backupWorkerCmd.Flags().IntVar(&backupWorkerBwLimit, "bwlimit", 0, "rsync --bwlimit in KB/s (0 = unthrottled)")
	backupWorkerCmd.Flags().StringSliceVar(&backupWorkerFilter, "node", nil, "restrict to this user@host (repeatable; default all entries in nodes.txt)")
	rootCmd.AddCommand(backupWorkerCmd)
	registerSkill("backup-worker", backupWorkerSkill)
}

func runBackupWorker(cmd *cobra.Command, _ []string) error {
	if _, err := execLookPath("rsync"); err != nil {
		return fmt.Errorf("backup-worker: rsync not on PATH: %w", err)
	}
	nodesPath, err := expandHomePath(backupWorkerNodesPath)
	if err != nil {
		return err
	}
	destRoot, err := expandHomePath(backupWorkerDestRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return fmt.Errorf("backup-worker: mkdir %s: %w", destRoot, err)
	}
	logPath, err := expandHomePath(backupWorkerLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(logPath)
	if err != nil {
		return fmt.Errorf("backup-worker: open log: %w", err)
	}
	defer logger.Close()

	targets, err := resolveBackupWorkerTargets(nodesPath, backupWorkerFilter)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errors.New("backup-worker: no workers to pull (nodes.txt empty or all filtered out)")
	}

	summary := backupWorkerSummary{Total: len(targets)}
	for _, t := range targets {
		start := time.Now()
		err := pullOneWorker(cmd.Context(), t, destRoot, backupWorkerSSHTimeout, backupWorkerBwLimit, backupWorkerDryRun)
		dur := time.Since(start).Round(time.Millisecond)
		if err != nil {
			summary.Failed++
			_ = logger.Action(log.Event{
				Node: t.Host, Phase: "backup-worker",
				Action: "rsync", Result: log.ResultError, DurationMs: dur.Milliseconds(),
				StderrTail: err.Error(),
			})
			summary.Lines = append(summary.Lines, fmt.Sprintf("  %-30s  FAILED  %s", t.String(), dur))
			continue
		}
		summary.OK++
		if !backupWorkerDryRun {
			markerPath := filepath.Join(destRoot, t.Host, ".LAST_PULL")
			_ = os.WriteFile(markerPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
		}
		_ = logger.Action(log.Event{
			Node: t.Host, Phase: "backup-worker",
			Action: "rsync", Result: log.ResultOK, DurationMs: dur.Milliseconds(),
		})
		summary.Lines = append(summary.Lines, fmt.Sprintf("  %-30s  ok      %s", t.String(), dur))
	}

	_ = logger.Action(log.Event{
		Node: "gateway", Phase: "backup-worker",
		Action: "run-complete", Result: log.ResultOK,
		StderrTail: fmt.Sprintf("total=%d ok=%d failed=%d dry_run=%t", summary.Total, summary.OK, summary.Failed, backupWorkerDryRun),
	})
	if !backupWorkerQuiet {
		fmt.Fprintf(cmd.OutOrStdout(),
			"fleetctl backup-worker: total=%d ok=%d failed=%d dry_run=%t\n%s\n",
			summary.Total, summary.OK, summary.Failed, backupWorkerDryRun, strings.Join(summary.Lines, "\n"))
	}
	if summary.Failed > 0 {
		return fmt.Errorf("backup-worker: %d/%d worker(s) failed", summary.Failed, summary.Total)
	}
	return nil
}

// backupWorkerSummary collects per-run counts + per-worker summary
// lines for the optional stderr printout.
type backupWorkerSummary struct {
	Total  int
	OK     int
	Failed int
	Lines  []string
}

// resolveBackupWorkerTargets reads nodes.txt (or honours the
// --node filter) and returns the parsed list. Filter matches
// user@host string-exact against the registry entries.
func resolveBackupWorkerTargets(nodesPath string, filter []string) ([]nodes.Node, error) {
	reg, err := nodes.Load(nodesPath)
	if err != nil {
		return nil, fmt.Errorf("backup-worker: load nodes: %w", err)
	}
	all := reg.All()
	if len(filter) == 0 {
		return all, nil
	}
	want := make(map[string]bool, len(filter))
	for _, f := range filter {
		want[strings.TrimSpace(f)] = true
	}
	out := make([]nodes.Node, 0, len(filter))
	for _, n := range all {
		if want[n.String()] {
			out = append(out, n)
		}
	}
	return out, nil
}

// pullOneWorker rsyncs the worker's ~/.hermes/ into destRoot/<host>/.
// On any non-zero rsync exit returns the error with stderr tail. The
// caller is responsible for logging + summary counting.
func pullOneWorker(ctx context.Context, n nodes.Node, destRoot string, sshTimeout time.Duration, bwLimit int, dryRun bool) error {
	dest := filepath.Join(destRoot, n.Host)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("mkdir dest %s: %w", dest, err)
	}
	sshArg := fmt.Sprintf("ssh -o BatchMode=yes -o ConnectTimeout=%d -o ServerAliveInterval=30",
		int(sshTimeout.Seconds()))
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		sshArg += " -o IdentityAgent=" + sock
	}
	args := []string{
		"-az",
		"--delete",
		"--partial",
		"--inplace",
		"-e", sshArg,
	}
	for _, ex := range rsyncExcludes {
		args = append(args, "--exclude="+ex)
	}
	if bwLimit > 0 {
		args = append(args, fmt.Sprintf("--bwlimit=%d", bwLimit))
	}
	if dryRun {
		args = append(args, "--dry-run", "-i")
	}
	// Trailing slash on src = copy *contents* of .hermes/, not the
	// directory itself. Matches the bash version's "src=node:/Users/hermes/.hermes/".
	src := n.String() + ":/Users/hermes/.hermes/"
	args = append(args, src, dest+"/")

	cmd := execCommandContext(ctx, "rsync", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync: %w (stderr=%s)", err, tailLines(stderr.String(), 4000))
	}
	return nil
}

// --- install / uninstall / status (mirror watchdog + logrotate + backup) ---

func oweraBackupWorkerPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", oweraBackupWorkerLabel+".plist"), nil
}

var newBackupWorkerManagerFn = func() *launchd.Manager { return launchd.New(nil) }

var backupWorkerInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install com.owera.backup-worker as a user LaunchAgent (daily 03:05 BRT)",
	RunE:  runBackupWorkerInstall,
}

func runBackupWorkerInstall(cmd *cobra.Command, _ []string) error {
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
	plistPath, err := oweraBackupWorkerPlistPath()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	m := newBackupWorkerManagerFn()
	body, err := m.RenderTemplate(oweraBackupWorkerTemplateName, launchd.Vars{
		Label:      oweraBackupWorkerLabel,
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
		"backup-worker: installed at %s (label=%s, exe=%s)\n"+
			"backup-worker: fires daily 03:05 local; force a run now with:\n"+
			"  launchctl kickstart -k gui/$(id -u)/%s\n",
		plistPath, oweraBackupWorkerLabel, exeAbs, oweraBackupWorkerLabel)
	return nil
}

var backupWorkerUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Tear down the com.owera.backup-worker LaunchAgent",
	RunE:  runBackupWorkerUninstall,
}

func runBackupWorkerUninstall(cmd *cobra.Command, _ []string) error {
	plistPath, err := oweraBackupWorkerPlistPath()
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	m := newBackupWorkerManagerFn()
	if err := m.Uninstall(cmd.Context(), oweraBackupWorkerLabel, plistPath); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"backup-worker: uninstalled (%s, label=%s)\n",
		plistPath, oweraBackupWorkerLabel)
	return nil
}

var backupWorkerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print the running state of com.owera.backup-worker",
	RunE:  runBackupWorkerStatus,
}

func runBackupWorkerStatus(cmd *cobra.Command, _ []string) error {
	m := newBackupWorkerManagerFn()
	st, err := m.Status(cmd.Context(), oweraBackupWorkerLabel)
	if err != nil {
		if isUnloadedErr(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "backup-worker: not loaded (label=%s)\n", oweraBackupWorkerLabel)
			return fmt.Errorf("not loaded")
		}
		return fmt.Errorf("status: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"backup-worker: label=%s loaded=%t pid=%s last_exit=%s state=%s\n",
		st.Label, st.Loaded, st.PID, st.LastExitCode, st.State)
	return nil
}

func init() {
	backupWorkerCmd.AddCommand(backupWorkerInstallCmd)
	backupWorkerCmd.AddCommand(backupWorkerUninstallCmd)
	backupWorkerCmd.AddCommand(backupWorkerStatusCmd)
}

func backupWorkerSkill() Skill {
	return Skill{
		Name:    "backup-worker",
		Trigger: "/backup-worker",
		Summary: "rsync each worker's ~/.hermes/ into the gateway tree (supersedes the hermes-setup bash com.hermes.backup-worker).",
		Args: []SkillArg{
			{Name: "--dry-run", Description: "Pass --dry-run -i to rsync; touch no destination files."},
			{Name: "--node USER@HOST", Description: "Restrict to this worker (repeatable; default all entries in nodes.txt)."},
			{Name: "--bwlimit KB/s", Description: "rsync --bwlimit in KB/s. 0 = unthrottled."},
			{Name: "--ssh-timeout DUR", Description: "rsync ssh -o ConnectTimeout value. Default 15s."},
			{Name: "--quiet", Description: "Suppress stderr summary."},
			{Name: "install / uninstall / status", Description: "launchd lifecycle for com.owera.backup-worker (daily 03:05)."},
		},
	}
}
