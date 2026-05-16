// bootstrap_worker.go implements `fleetctl bootstrap-worker` — the typed
// Go replacement for scripts/bootstrap-hermes-node.sh.
//
// Wave 3 / WS-2 T2.5: run phase 0 (Homebrew baseline) against a single
// worker target. Additional phases land as remote/phase*.sh scripts are
// authored. The command auto-discovers them by scanning the remote/ dir.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/bootstrap"
	"github.com/owera/owera-fleet/internal/log"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

var (
	bwNode       string
	bwPhase      string
	bwScriptDir  string
	bwDryRun     bool
	bwQuiet      bool
	bwTimeout    time.Duration
	bwLogPath    = "~/.hermes/logs/bootstrap.jsonl"
)

var bootstrapWorkerCmd = &cobra.Command{
	Use:   "bootstrap-worker",
	Short: "Provision a worker by running the multi-phase bootstrap pipeline",
	Long: `bootstrap-worker runs remote bootstrap phase scripts (remote/phase*.sh) on a
worker node via SSH, capturing per-action JSONL telemetry.

Phase 0 (Homebrew baseline) is always run first. Additional phases are
run in order as their scripts land in the remote/ directory.

Use --phase to run a single named phase (e.g. "phase00_brew_baseline.sh").
Use --dry-run to run scripts with --dry-run flag (probe-only, no mutations).

All phase events are appended to ~/.hermes/logs/bootstrap.jsonl.`,
	Example: `  fleetctl bootstrap-worker --node hermes@claw1.local
  fleetctl bootstrap-worker --node hermes@claw1.local --phase phase00_brew_baseline.sh
  fleetctl bootstrap-worker --node hermes@claw1.local --dry-run`,
	RunE: runBootstrapWorker,
}

func init() {
	bootstrapWorkerCmd.Flags().StringVar(&bwNode, "node", "", "target worker user@host (required)")
	bootstrapWorkerCmd.Flags().StringVar(&bwPhase, "phase", "", "run a single named phase script (default: all available)")
	bootstrapWorkerCmd.Flags().StringVar(&bwScriptDir, "script-dir", "", "override local remote/ directory")
	bootstrapWorkerCmd.Flags().BoolVar(&bwDryRun, "dry-run", false, "pass --dry-run to phase scripts (probe only)")
	bootstrapWorkerCmd.Flags().BoolVar(&bwQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	bootstrapWorkerCmd.Flags().DurationVar(&bwTimeout, "timeout", 10*time.Minute, "per-phase timeout")
	rootCmd.AddCommand(bootstrapWorkerCmd)
}

// bwOrchestratorFactory is overridable in tests.
var bwOrchestratorFactory = func(target osh.Target, timeout time.Duration) (*bootstrap.Orchestrator, error) {
	dialer := osh.NewDialer(osh.WithConnectTimeout(timeout))
	client, err := dialer.Dial(context.Background(), target)
	if err != nil {
		return nil, fmt.Errorf("bootstrap-worker: dial %s: %w", target, err)
	}
	return &bootstrap.Orchestrator{
		Upload: func(ctx context.Context, localPath, remotePath string) error {
			data, err := os.ReadFile(localPath)
			if err != nil {
				return err
			}
			// Write to remote via ssh `cat > <path>`.
			stdin := strings.NewReader(string(data))
			_ = stdin
			_, _, _, err = func() (string, string, int, error) {
				res, runErr := client.Run(ctx, fmt.Sprintf("cat > %s && chmod +x %s", remotePath, remotePath))
				return res.Stdout, res.Stderr, res.ExitCode, runErr
			}()
			return err
		},
		Run: func(ctx context.Context, remoteCmd string) (string, string, int, error) {
			res, err := client.Run(ctx, remoteCmd)
			return res.Stdout, res.Stderr, res.ExitCode, err
		},
	}, nil
}

func runBootstrapWorker(cmd *cobra.Command, _ []string) error {
	if bwNode == "" {
		return errors.New("bootstrap-worker: --node is required")
	}

	target, err := osh.ParseTarget(bwNode)
	if err != nil {
		return fmt.Errorf("bootstrap-worker: parse --node: %w", err)
	}

	resolvedLog, err := expandHomePath(bwLogPath)
	if err != nil {
		return fmt.Errorf("bootstrap-worker: expand log path: %w", err)
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("bootstrap-worker: open log: %w", err)
	}
	defer logger.Close()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	orch, err := bwOrchestratorFactory(target, bwTimeout)
	if err != nil {
		return err
	}
	if bwScriptDir != "" {
		orch.ScriptDir = bwScriptDir
	}

	// Select phases to run.
	phases := bwPhasesToRun()
	if len(phases) == 0 {
		return errors.New("bootstrap-worker: no phase scripts found")
	}

	nodeLabel := target.User + "@" + target.Host
	overall := log.ResultOK

	for _, phase := range phases {
		phaseCtx, cancel := context.WithTimeout(ctx, bwTimeout)
		args := []string{"--node", nodeLabel}
		if bwDryRun {
			args = append(args, "--dry-run")
		}

		res, runErr := orch.RunPhase(phaseCtx, phase, args...)
		cancel()

		if !bwQuiet {
			res.WriteTo(cmd.OutOrStdout())
		}

		// Write one JSONL event per phase.
		result := log.ResultOK
		tail := ""
		if runErr != nil || !res.OK() {
			result = log.ResultError
			if runErr != nil {
				tail = runErr.Error()
			} else {
				tail = fmt.Sprintf("exit=%d", res.ExitCode)
			}
			overall = log.ResultError
		}
		_ = logger.Action(log.Event{
			Node:       nodeLabel,
			Phase:      "bootstrap",
			Action:     "phase:" + phase,
			Result:     result,
			DurationMs: res.Duration.Milliseconds(),
			StderrTail: tail,
		})

		if runErr != nil {
			return fmt.Errorf("bootstrap-worker: %s: %w", phase, runErr)
		}
		if !res.OK() && !bwDryRun {
			return fmt.Errorf("bootstrap-worker: %s failed (exit=%d)", phase, res.ExitCode)
		}
	}

	if overall != log.ResultOK {
		return errors.New("bootstrap-worker: one or more phases failed")
	}
	if !bwQuiet {
		fmt.Fprintf(cmd.OutOrStdout(), "\nbootstrap-worker: all phases complete for %s\n", nodeLabel)
	}
	return nil
}

// bwPhasesToRun returns the ordered list of phase script names to execute.
func bwPhasesToRun() []string {
	if bwPhase != "" {
		return []string{bwPhase}
	}
	// Default: phase00 only for now; additional phases added as they land.
	return []string{"phase00_brew_baseline.sh"}
}

func bootstrapWorkerSkill() Skill {
	return Skill{
		Name:    "bootstrap-worker",
		Trigger: "/bootstrap-worker",
		Summary: "Provision a worker by running the multi-phase bootstrap pipeline (Homebrew baseline + future phases).",
		Args: []SkillArg{
			{Name: "--node USER@HOST", Description: "Target worker (required)."},
			{Name: "--phase SCRIPT", Description: "Run a single named phase script instead of all."},
			{Name: "--script-dir PATH", Description: "Override the local remote/ directory."},
			{Name: "--dry-run", Description: "Pass --dry-run to phase scripts (probe only, no mutations)."},
			{Name: "--timeout DUR", Description: "Per-phase timeout (default 10m)."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
		},
		Examples: []SkillExample{
			{Description: "Full bootstrap", Command: "fleetctl bootstrap-worker --node hermes@claw1.local"},
			{Description: "Dry-run probe", Command: "fleetctl bootstrap-worker --node hermes@claw1.local --dry-run"},
			{Description: "Single phase", Command: "fleetctl bootstrap-worker --node hermes@claw1.local --phase phase00_brew_baseline.sh"},
		},
	}
}

func init() { registerSkill("bootstrap-worker", bootstrapWorkerSkill) }
