// update.go implements `fleetctl update` — checks and optionally upgrades the
// hermes binary on one, all, or a random worker via SSH.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

var (
	updateNode      string
	updateAll       bool
	updateDryRun    bool
	updateVersion   string
	updateQuiet     bool
	updateTimeout   time.Duration
	updateLogPath   = "~/.hermes/logs/update.jsonl"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Check or upgrade the hermes binary on fleet workers",
	Long: `update checks the current hermes version on each target worker via SSH and
optionally runs the upgrade command. Without --node or --all it only checks
the gateway-local hermes binary.

--dry-run reports what would be installed without making changes.
Every probe or upgrade action is appended to ~/.hermes/logs/update.jsonl.`,
	Example: `  fleetctl update --dry-run
  fleetctl update --all
  fleetctl update --node hermes@claw1.local
  fleetctl update --all --version v0.14.0`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().StringVar(&updateNode, "node", "", "target worker user@host")
	updateCmd.Flags().BoolVar(&updateAll, "all", false, "update every entry in ~/.hermes/nodes.txt")
	updateCmd.Flags().BoolVar(&updateDryRun, "dry-run", false, "report version status without making changes")
	updateCmd.Flags().StringVar(&updateVersion, "version", "", "pin to a specific hermes version (default: latest)")
	updateCmd.Flags().BoolVar(&updateQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	updateCmd.Flags().DurationVar(&updateTimeout, "timeout", 60*time.Second, "per-node SSH cap")
	rootCmd.AddCommand(updateCmd)
}

// updateDialer is overridable in tests.
var updateDialer = func(opts ...osh.Option) Dialer {
	return realDialer{d: osh.NewDialer(opts...)}
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	if updateNode != "" && updateAll {
		return errors.New("update: --node and --all are mutually exclusive")
	}

	resolvedLog, err := expandHomePath(updateLogPath)
	if err != nil {
		return fmt.Errorf("update: expand log path: %w", err)
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("update: open log: %w", err)
	}
	defer logger.Close()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	targets, err := resolveUpdateTargets()
	if err != nil {
		return err
	}

	dialer := updateDialer(osh.WithConnectTimeout(updateTimeout))
	overall := 0
	for _, t := range targets {
		if exit := dispatchUpdate(ctx, cmd, dialer, logger, t); exit != 0 && overall == 0 {
			overall = exit
		}
	}
	if overall != 0 {
		return errors.New("update: one or more targets failed")
	}
	return nil
}

func dispatchUpdate(ctx context.Context, cmd *cobra.Command, dialer Dialer, logger *log.Logger, target osh.Target) int {
	nodeLabel := target.User + "@" + target.Host
	start := time.Now()

	dialCtx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	client, err := dialer.Dial(dialCtx, target)
	if err != nil {
		dur := time.Since(start).Milliseconds()
		updateLog(logger, nodeLabel, "dial", log.ResultError, dur, err.Error())
		if !updateQuiet {
			fmt.Fprintf(cmd.ErrOrStderr(), "FAIL %s — dial: %v\n", nodeLabel, err)
		}
		return 1
	}
	defer client.Close()

	// Check current version.
	res, runErr := client.Run(dialCtx, "hermes --version 2>&1 | head -1")
	if runErr != nil {
		dur := time.Since(start).Milliseconds()
		updateLog(logger, nodeLabel, "version-check", log.ResultError, dur, runErr.Error())
		if !updateQuiet {
			fmt.Fprintf(cmd.ErrOrStderr(), "FAIL %s — version check: %v\n", nodeLabel, runErr)
		}
		return 1
	}
	currentVer := strings.TrimSpace(res.Stdout)
	dur := time.Since(start).Milliseconds()

	if !updateQuiet {
		fmt.Fprintf(cmd.OutOrStdout(), "%s  current: %s\n", nodeLabel, currentVer)
	}

	if updateDryRun {
		target := "latest"
		if updateVersion != "" {
			target = updateVersion
		}
		updateLog(logger, nodeLabel, "version-check:dry-run", log.ResultOK, dur, "current="+currentVer+" would-install="+target)
		return 0
	}

	// Run upgrade.
	upgradeCmd := "brew upgrade hermes 2>&1"
	if updateVersion != "" {
		upgradeCmd = fmt.Sprintf("brew install hermes@%s 2>&1", updateVersion)
	}
	res2, upgradeErr := client.Run(dialCtx, upgradeCmd)
	dur2 := time.Since(start).Milliseconds()
	if upgradeErr != nil || res2.ExitCode != 0 {
		tail := strings.TrimSpace(res2.Stderr)
		if upgradeErr != nil && tail == "" {
			tail = upgradeErr.Error()
		}
		updateLog(logger, nodeLabel, "upgrade", log.ResultError, dur2, tail)
		if !updateQuiet {
			fmt.Fprintf(cmd.ErrOrStderr(), "FAIL %s — upgrade: %v\n", nodeLabel, upgradeErr)
		}
		return 1
	}
	updateLog(logger, nodeLabel, "upgrade", log.ResultOK, dur2, "")
	if !updateQuiet {
		fmt.Fprintf(cmd.OutOrStdout(), "OK   %s  upgraded\n", nodeLabel)
	}
	return 0
}

func updateLog(logger *log.Logger, node, action string, result log.Result, dur int64, tail string) {
	if logger == nil {
		return
	}
	hostname, _ := os.Hostname()
	_ = hostname
	_ = logger.Action(log.Event{
		Node:       node,
		Phase:      "update",
		Action:     action,
		Result:     result,
		DurationMs: dur,
		StderrTail: tail,
	})
}

func resolveUpdateTargets() ([]osh.Target, error) {
	if updateNode != "" {
		t, err := osh.ParseTarget(updateNode)
		if err != nil {
			return nil, fmt.Errorf("update: parse --node: %w", err)
		}
		return []osh.Target{t}, nil
	}
	if updateAll {
		reg, err := nodes.Load("")
		if err != nil {
			return nil, fmt.Errorf("update: load nodes: %w", err)
		}
		all := reg.All()
		out := make([]osh.Target, 0, len(all))
		for _, n := range all {
			t, err := osh.ParseTarget(n.String())
			if err != nil {
				return nil, fmt.Errorf("update: parse %s: %w", n, err)
			}
			out = append(out, t)
		}
		return out, nil
	}
	// No node specified — check gateway's local hermes (no SSH needed).
	return nil, nil
}

func updateSkill() Skill {
	return Skill{
		Name:    "update",
		Trigger: "/update",
		Summary: "Check or upgrade the hermes binary on fleet workers via SSH.",
		Args: []SkillArg{
			{Name: "--node USER@HOST", Description: "Target one worker."},
			{Name: "--all", Description: "Target every node in ~/.hermes/nodes.txt."},
			{Name: "--dry-run", Description: "Report version status without making changes."},
			{Name: "--version VERSION", Description: "Pin to a specific hermes version."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
			{Name: "--timeout DUR", Description: "Per-node SSH cap (default 60s)."},
		},
		Examples: []SkillExample{
			{Description: "Dry-run check across the fleet", Command: "fleetctl update --all --dry-run"},
			{Description: "Upgrade one worker", Command: "fleetctl update --node hermes@claw1.local"},
			{Description: "Upgrade to specific version", Command: "fleetctl update --all --version v0.14.0"},
		},
	}
}

func init() { registerSkill("update", updateSkill) }
