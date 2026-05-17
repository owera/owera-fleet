// heartbeats_bridge.go bridges the legacy worker-side heartbeat
// convention to the new gateway-side one that fleet.HealthSnapshot
// reads.
//
// # Background
//
// hermes-setup era: each worker runs a per-host LaunchDaemon
// (com.hermes.heartbeat) that updates `~/.hermes/heartbeat/<host>`
// (no extension, file body is the epoch seconds) every 60s.
//
// owera-fleet era: `internal/metrics.HeartbeatAges` (the source of
// truth fleet.HealthSnapshot + Prometheus both consult) reads
// `~/.hermes/heartbeats/*.json` on the GATEWAY and uses mtime as the
// freshness signal — see metrics/metrics.go.
//
// The two paths don't overlap. heartbeats-bridge closes the gap:
// every --interval it SSH-probes each worker in ~/.hermes/nodes.txt,
// reads the mtime of the worker's heartbeat file, and if it's within
// --stale-max, touches `~/.hermes/heartbeats/<host>.json` on the
// gateway. fleet.HealthSnapshot then reports the worker as ok=true.
//
// # Design choices
//
//   - The bridge does NOT pull the worker's heartbeat file contents.
//     It only checks mtime via `stat -f %m <path>` (or `stat -c %Y`
//     on linux, mirroring the legacy script's heuristic). The local
//     <host>.json's mtime is the answer to "when did we last see
//     this worker alive?" — using the local clock keeps the read
//     path one-source-of-truth.
//
//   - One-shot mode (`--once`) for ad-hoc operator commands +
//     CI smoke. Long-running mode (default) loops on --interval.
//     Independent per-cycle errors are logged + counted but do not
//     halt the loop.
//
//   - Install / uninstall / status subcommands mirror the
//     snapshot-publish pattern so the lifecycle UX is consistent
//     across operator daemons.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/launchd"
	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
	"github.com/owera/owera-fleet/internal/ssh"
)

var (
	bridgeInterval        time.Duration
	bridgeStaleMax        time.Duration
	bridgeOnce            bool
	bridgeNodesPath       = "~/.hermes/nodes.txt"
	bridgeHeartbeatsDir   = "~/.hermes/heartbeats"
	bridgeWorkerSrcDir    = "~/.hermes/heartbeat" // legacy worker-side dir
	bridgeLogPath         = "~/.hermes/logs/heartbeats-bridge.jsonl"
	bridgeSSHTimeout      time.Duration
)

const (
	heartbeatsBridgeLabel        = "com.owera.heartbeats-bridge"
	heartbeatsBridgeTemplateName = "heartbeats-bridge"
)

var heartbeatsBridgeCmd = &cobra.Command{
	Use:   "heartbeats-bridge",
	Short: "Refresh ~/.hermes/heartbeats/<host>.json from per-worker SSH probes",
	Long: `heartbeats-bridge SSH-probes every worker in ~/.hermes/nodes.txt
and, if the worker's legacy heartbeat file has been written recently
(within --stale-max), touches ~/.hermes/heartbeats/<host>.json so
fleet.HealthSnapshot reports the worker as alive.

Defaults:
  --interval     60s
  --stale-max    5m     (matches the legacy watchdog threshold)
  --ssh-timeout  10s    (per-worker probe ceiling)

The process runs until SIGINT/SIGTERM. Pass --once to run a single
sweep and exit non-zero if any worker failed.`,
	Example: `  fleetctl heartbeats-bridge
  fleetctl heartbeats-bridge --interval 30s
  fleetctl heartbeats-bridge --once  # smoke check`,
	RunE: runHeartbeatsBridge,
}

func init() {
	heartbeatsBridgeCmd.Flags().DurationVar(&bridgeInterval, "interval", 60*time.Second, "poll interval (≥10s)")
	heartbeatsBridgeCmd.Flags().DurationVar(&bridgeStaleMax, "stale-max", 5*time.Minute, "treat worker heartbeats older than this as dead")
	heartbeatsBridgeCmd.Flags().DurationVar(&bridgeSSHTimeout, "ssh-timeout", 10*time.Second, "per-worker SSH probe timeout")
	heartbeatsBridgeCmd.Flags().BoolVar(&bridgeOnce, "once", false, "run a single sweep and exit (exit non-zero on any worker failure)")
	rootCmd.AddCommand(heartbeatsBridgeCmd)
	registerSkill("heartbeats-bridge", heartbeatsBridgeSkill)
}

func runHeartbeatsBridge(cmd *cobra.Command, _ []string) error {
	if bridgeInterval < 10*time.Second {
		return fmt.Errorf("heartbeats-bridge: interval %s < 10s", bridgeInterval)
	}
	nodesPath, err := expandHomePath(bridgeNodesPath)
	if err != nil {
		return err
	}
	heartbeatsDir, err := expandHomePath(bridgeHeartbeatsDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(heartbeatsDir, 0o755); err != nil {
		return fmt.Errorf("heartbeats-bridge: mkdir %s: %w", heartbeatsDir, err)
	}
	logPath, err := expandHomePath(bridgeLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(logPath)
	if err != nil {
		return fmt.Errorf("heartbeats-bridge: open log: %w", err)
	}
	defer logger.Close()

	if bridgeOnce {
		return sweepOnce(cmd.Context(), nodesPath, heartbeatsDir, bridgeStaleMax, bridgeSSHTimeout, logger)
	}
	return sweepLoop(cmd, nodesPath, heartbeatsDir, bridgeStaleMax, bridgeSSHTimeout, bridgeInterval, logger)
}

// sweepOnce runs one full sweep across all nodes. Returns an error if
// ANY node fails — useful for --once smoke checks.
func sweepOnce(ctx context.Context, nodesPath, heartbeatsDir string, staleMax, sshTimeout time.Duration, logger *log.Logger) error {
	reg, err := nodes.Load(nodesPath)
	if err != nil {
		return fmt.Errorf("heartbeats-bridge: load nodes: %w", err)
	}
	var lastErr error
	for _, n := range reg.All() {
		if err := bridgeOne(ctx, n, heartbeatsDir, staleMax, sshTimeout, logger); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// sweepLoop runs sweeps on a ticker until SIGINT/SIGTERM. Failures are
// logged + counted (consecutiveFailures across the whole loop, not
// per-node) but never halt the loop.
func sweepLoop(cmd *cobra.Command, nodesPath, heartbeatsDir string, staleMax, sshTimeout, interval time.Duration, logger *log.Logger) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var consecutiveFailures atomic.Int64
	sweep := func() {
		ctx, cancel := context.WithTimeout(cmd.Context(), interval-time.Second)
		defer cancel()
		reg, err := nodes.Load(nodesPath)
		if err != nil {
			consecutiveFailures.Add(1)
			_ = logger.Action(log.Event{
				Node: "gateway", Phase: "heartbeats-bridge",
				Action: "load-nodes", Result: log.ResultError, StderrTail: err.Error(),
			})
			return
		}
		anyFail := false
		for _, n := range reg.All() {
			if err := bridgeOne(ctx, n, heartbeatsDir, staleMax, sshTimeout, logger); err != nil {
				anyFail = true
			}
		}
		if anyFail {
			consecutiveFailures.Add(1)
		} else {
			consecutiveFailures.Store(0)
		}
	}

	sweep() // immediate first sweep
	fmt.Fprintf(cmd.OutOrStdout(), "fleetctl heartbeats-bridge: writing %s every %s (stale-max %s)\n",
		heartbeatsDir, interval, staleMax)
	for {
		select {
		case <-sigCh:
			fmt.Fprintln(cmd.OutOrStdout(), "fleetctl heartbeats-bridge: shutting down")
			return nil
		case <-ticker.C:
			sweep()
		}
	}
}

// bridgeOne probes one worker. On success (worker heartbeat is fresh
// within staleMax), touches the local <host>.json with the current
// time. On any failure (SSH down, file missing, file stale), logs but
// does NOT remove the local file — staleness shows up naturally as
// the local mtime ages past whatever consumer threshold reads it.
func bridgeOne(ctx context.Context, n nodes.Node, heartbeatsDir string, staleMax, sshTimeout time.Duration, logger *log.Logger) error {
	probeCtx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()

	// `stat -f %m` on BSD/macOS, `stat -c %Y` on Linux. Workers are
	// macOS in V0, so the BSD form is correct; the legacy watchdog
	// shell script used the same flag.
	target, err := ssh.ParseTarget(n.String())
	if err != nil {
		return fmt.Errorf("parse target %s: %w", n.String(), err)
	}
	dialer := ssh.NewDialer(ssh.WithConnectTimeout(sshTimeout))
	client, err := dialer.Dial(probeCtx, target)
	if err != nil {
		_ = logger.Action(log.Event{
			Node: n.Host, Phase: "heartbeats-bridge",
			Action: "ssh-dial", Result: log.ResultError, StderrTail: err.Error(),
		})
		return fmt.Errorf("probe %s: %w", n.Host, err)
	}
	defer client.Close()
	remotePath := fmt.Sprintf("~/.hermes/heartbeat/%s", n.Host)
	res, err := client.Run(probeCtx, fmt.Sprintf("stat -f %%m %s", remotePath))
	if err != nil || res.ExitCode != 0 {
		stderrTail := ""
		if err != nil {
			stderrTail = err.Error()
		} else {
			stderrTail = fmt.Sprintf("exit %d: %s", res.ExitCode, res.Stderr)
		}
		_ = logger.Action(log.Event{
			Node: n.Host, Phase: "heartbeats-bridge",
			Action: "ssh-stat", Result: log.ResultError, StderrTail: stderrTail,
		})
		return fmt.Errorf("probe %s: %s", n.Host, stderrTail)
	}
	epochStr := strings.TrimSpace(res.Stdout)
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		_ = logger.Action(log.Event{
			Node: n.Host, Phase: "heartbeats-bridge",
			Action: "parse-mtime", Result: log.ResultError, StderrTail: fmt.Sprintf("output %q: %v", epochStr, err),
		})
		return fmt.Errorf("parse %s mtime: %w", n.Host, err)
	}
	hbTime := time.Unix(epoch, 0)
	age := time.Since(hbTime)
	if age > staleMax {
		_ = logger.Action(log.Event{
			Node: n.Host, Phase: "heartbeats-bridge",
			Action: "stale", Result: log.ResultWarn,
			StderrTail: fmt.Sprintf("age %s > stale-max %s", age, staleMax),
		})
		return fmt.Errorf("%s heartbeat stale by %s", n.Host, age)
	}

	outPath := filepath.Join(heartbeatsDir, n.Host+".json")
	// Body: tiny JSON for operator-debug; mtime is what consumers read.
	body := fmt.Sprintf("{\"host\":%q,\"worker_epoch\":%d,\"observed_at\":%q}\n",
		n.Host, epoch, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(outPath, []byte(body), 0o644); err != nil {
		_ = logger.Action(log.Event{
			Node: n.Host, Phase: "heartbeats-bridge",
			Action: "write:" + outPath, Result: log.ResultError, StderrTail: err.Error(),
		})
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	_ = logger.Action(log.Event{
		Node: n.Host, Phase: "heartbeats-bridge",
		Action: "fresh", Result: log.ResultOK,
	})
	return nil
}

// --- install / uninstall / status (mirror snapshot-publish pattern) ---

func heartbeatsBridgePlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", heartbeatsBridgeLabel+".plist"), nil
}

// newHeartbeatsBridgeManagerFn lets tests substitute a Manager that
// mocks RunCmd / WriteFile.
var newHeartbeatsBridgeManagerFn = func() *launchd.Manager { return launchd.New(nil) }

var heartbeatsBridgeInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install com.owera.heartbeats-bridge as a user LaunchAgent",
	RunE:  runHeartbeatsBridgeInstall,
}

func runHeartbeatsBridgeInstall(cmd *cobra.Command, _ []string) error {
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
	plistPath, err := heartbeatsBridgePlistPath()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	uid := os.Getuid()
	if u, err := user.Current(); err == nil {
		if parsed, err := strconv.Atoi(u.Uid); err == nil {
			uid = parsed
		}
	}
	m := newHeartbeatsBridgeManagerFn()
	body, err := m.RenderTemplate(heartbeatsBridgeTemplateName, launchd.Vars{
		Label:      heartbeatsBridgeLabel,
		UID:        uid,
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
		"heartbeats-bridge: installed at %s (label=%s, exe=%s)\n",
		plistPath, heartbeatsBridgeLabel, exeAbs)
	return nil
}

var heartbeatsBridgeUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Tear down the com.owera.heartbeats-bridge LaunchAgent",
	RunE:  runHeartbeatsBridgeUninstall,
}

func runHeartbeatsBridgeUninstall(cmd *cobra.Command, _ []string) error {
	plistPath, err := heartbeatsBridgePlistPath()
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	m := newHeartbeatsBridgeManagerFn()
	if err := m.Uninstall(cmd.Context(), heartbeatsBridgeLabel, plistPath); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"heartbeats-bridge: uninstalled (%s, label=%s)\n",
		plistPath, heartbeatsBridgeLabel)
	return nil
}

var heartbeatsBridgeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print the running state of com.owera.heartbeats-bridge",
	RunE:  runHeartbeatsBridgeStatus,
}

func runHeartbeatsBridgeStatus(cmd *cobra.Command, _ []string) error {
	m := newHeartbeatsBridgeManagerFn()
	st, err := m.Status(cmd.Context(), heartbeatsBridgeLabel)
	if err != nil {
		if errors.Is(err, launchd.ErrUnloaded) {
			fmt.Fprintf(cmd.OutOrStdout(), "heartbeats-bridge: not loaded (label=%s)\n", heartbeatsBridgeLabel)
			return fmt.Errorf("not loaded")
		}
		return fmt.Errorf("status: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"heartbeats-bridge: label=%s loaded=%t pid=%s last_exit=%s state=%s\n",
		st.Label, st.Loaded, st.PID, st.LastExitCode, st.State)
	return nil
}

func init() {
	heartbeatsBridgeCmd.AddCommand(heartbeatsBridgeInstallCmd)
	heartbeatsBridgeCmd.AddCommand(heartbeatsBridgeUninstallCmd)
	heartbeatsBridgeCmd.AddCommand(heartbeatsBridgeStatusCmd)
}

func heartbeatsBridgeSkill() Skill {
	return Skill{
		Name:    "heartbeats-bridge",
		Trigger: "/heartbeats-bridge",
		Summary: "Refresh ~/.hermes/heartbeats/<host>.json from per-worker SSH probes so fleet.HealthSnapshot sees them.",
		Args: []SkillArg{
			{Name: "--interval DUR", Description: "Poll interval (≥10s). Default 60s."},
			{Name: "--stale-max DUR", Description: "Treat worker heartbeats older than this as dead. Default 5m."},
			{Name: "--ssh-timeout DUR", Description: "Per-worker SSH probe timeout. Default 10s."},
			{Name: "--once", Description: "Run a single sweep and exit (CI / ad-hoc shape)."},
			{Name: "install / uninstall / status", Description: "launchd lifecycle for the daemon."},
		},
	}
}
