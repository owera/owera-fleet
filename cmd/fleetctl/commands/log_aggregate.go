// log_aggregate.go implements `fleetctl log-aggregate` — the WS-10 nightly
// log-aggregation primitive. The gateway pulls each worker's
// ~/.hermes/logs/*.jsonl over SSH (tar+gzip; never rsync — see the package
// doc on [internal/logaggregate] for why), lands them under
// ~/.hermes/logs/aggregated/<host>/, and optionally rotates the source
// files on each worker.
//
// One JSONL event per node lands in ~/.hermes/logs/log-aggregate.jsonl
// with phase="log-aggregate", so the audit roll-up sees aggregation
// without a special case.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/logaggregate"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

var logAggregateLogPath = "~/.hermes/logs/log-aggregate.jsonl"

// logAggregateNodesPath defaults to nodes.Load(""), which resolves to
// ~/.hermes/nodes.txt. Tests override this with a fixture path.
var logAggregateNodesPath = ""

// logAggregateGatewayDir is the per-host landing root. Lives under
// ~/.hermes/logs/aggregated/ so the daily logrotate agent doesn't catch
// it (logrotate only walks the top-level logs/, not subdirectories).
var logAggregateGatewayDir = "~/.hermes/logs/aggregated"

var (
	logAggregateNode         string
	logAggregateKeepDays     int
	logAggregateJSON         bool
	logAggregateQuiet        bool
	logAggregateDryRun       bool
	logAggregateRotateOnPull bool
	logAggregateTimeout      time.Duration
)

var logAggregateCobraCmd = &cobra.Command{
	Use:   "log-aggregate",
	Short: "Pull every worker's ~/.hermes/logs/*.jsonl to the gateway",
	Long: `log-aggregate fans out to every entry in ~/.hermes/nodes.txt, pulls each
worker's ~/.hermes/logs/*.jsonl over SSH (tar+gzip; never rsync), and
lands them under ~/.hermes/logs/aggregated/<host>/. Files older than
--keep-days are pruned from the per-host landing directories.

One JSONL event per node is appended to ~/.hermes/logs/log-aggregate.jsonl
with phase="log-aggregate". The aggregator is wired to a LaunchAgent
(com.hermes.logaggregate) that fires daily at 03:45 — between the 03:15
backup and the 03:30 logrotate.`,
	Example: `  fleetctl log-aggregate
  fleetctl log-aggregate --node hermes@claw1.local
  fleetctl log-aggregate --keep-days 14 --rotate-on-pull
  fleetctl log-aggregate --dry-run --json`,
	RunE: runLogAggregate,
}

func init() {
	logAggregateCobraCmd.Flags().StringVar(&logAggregateNode, "node", "", "pull from one user@host only (default: every entry in nodes.txt)")
	logAggregateCobraCmd.Flags().IntVar(&logAggregateKeepDays, "keep-days", 30, "retention window for files under the landing dir (negative disables pruning)")
	logAggregateCobraCmd.Flags().BoolVar(&logAggregateJSON, "json", false, "emit one JSON object per pull to stdout")
	logAggregateCobraCmd.Flags().BoolVar(&logAggregateQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	logAggregateCobraCmd.Flags().BoolVar(&logAggregateDryRun, "dry-run", false, "probe each worker but skip extract/prune/rotate")
	logAggregateCobraCmd.Flags().BoolVar(&logAggregateRotateOnPull, "rotate-on-pull", false, "gzip the source *.jsonl files on each worker after a successful pull")
	logAggregateCobraCmd.Flags().DurationVar(&logAggregateTimeout, "timeout", 5*time.Minute, "per-node SSH connect + tar/extract cap")
	rootCmd.AddCommand(logAggregateCobraCmd)
}

// newLogAggregateRunner is overridable in tests; production wraps the
// SSH dialer into the SSHRunner shape the aggregator consumes.
var newLogAggregateRunner = func(timeout time.Duration) logaggregate.SSHRunner {
	dialer := newDialer(osh.WithConnectTimeout(timeout))
	return func(ctx context.Context, target string, cmd string) (string, string, int, error) {
		t, err := osh.ParseTarget(target)
		if err != nil {
			return "", err.Error(), -1, fmt.Errorf("parse target %q: %w", target, err)
		}
		client, err := dialer.Dial(ctx, t, osh.WithConnectTimeout(timeout))
		if err != nil {
			return "", err.Error(), -1, err
		}
		defer client.Close()
		res, runErr := client.Run(ctx, cmd)
		if runErr != nil {
			return res.Stdout, res.Stderr, res.ExitCode, runErr
		}
		return res.Stdout, res.Stderr, res.ExitCode, nil
	}
}

func runLogAggregate(cmd *cobra.Command, _ []string) error {
	resolvedLog, err := expandHomePath(logAggregateLogPath)
	if err != nil {
		return fmt.Errorf("log-aggregate: expand log path: %w", err)
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("log-aggregate: open log: %w", err)
	}
	defer logger.Close()

	resolvedGateway, err := expandHomePath(logAggregateGatewayDir)
	if err != nil {
		return fmt.Errorf("log-aggregate: expand gateway dir: %w", err)
	}

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	agg := &logaggregate.Aggregator{
		GatewayLogDir: resolvedGateway,
		SSHRunner:     newLogAggregateRunner(logAggregateTimeout),
		NodesFile:     logAggregateNodesPath,
		SingleNode:    logAggregateNode,
		KeepDays:      logAggregateKeepDays,
		RotateOnPull:  logAggregateRotateOnPull,
		DryRun:        logAggregateDryRun,
	}

	res, runErr := agg.Run(parent)
	if runErr != nil {
		// A pre-flight error (no SSHRunner, no nodes, etc.) — log a
		// single error event so the audit stream doesn't go silent.
		_ = logger.Action(log.Event{
			Phase:      "log-aggregate",
			Action:     "preflight",
			Result:     log.ResultError,
			StderrTail: runErr.Error(),
		})
		cmd.SilenceUsage = true
		return runErr
	}

	for _, p := range res.Pulls {
		result := log.ResultOK
		tail := ""
		action := fmt.Sprintf("pull files=%d bytes=%d pruned=%d", p.FilesPulled, p.BytesPulled, p.FilesPruned)
		if logAggregateDryRun {
			action = fmt.Sprintf("dry-run files=%d", p.FilesPulled)
		}
		if p.Err != nil {
			result = log.ResultError
			tail = p.Err.Error()
		}
		_ = logger.Action(log.Event{
			Node:       p.Node,
			Phase:      "log-aggregate",
			Action:     action,
			Result:     result,
			DurationMs: p.Duration.Milliseconds(),
			StderrTail: tail,
		})
		writeLogAggregateOutput(cmd, p)
	}

	if !res.OK {
		cmd.SilenceUsage = true
		return fmt.Errorf("log-aggregate: one or more pulls failed")
	}
	return nil
}

// writeLogAggregateOutput renders one PullResult to stdout in the
// flavour --json / --quiet / human dictate. Pattern matches delegate.
func writeLogAggregateOutput(cmd *cobra.Command, p logaggregate.PullResult) {
	if logAggregateQuiet {
		return
	}
	stdout := cmd.OutOrStdout()
	if logAggregateJSON {
		// Re-marshal so the Err interface is rendered as the canonical
		// ErrMessage string (PullResult.Err has json:"-").
		buf, _ := json.Marshal(p)
		fmt.Fprintln(stdout, string(buf))
		return
	}
	status := "ok"
	if p.Err != nil {
		status = "error: " + p.Err.Error()
	}
	if logAggregateDryRun {
		fmt.Fprintf(stdout, "%-30s %s  (would pull %d files)\n", p.Node, status, p.FilesPulled)
		return
	}
	fmt.Fprintf(stdout, "%-30s %s  pulled=%d bytes=%d pruned=%d duration=%s\n",
		p.Node, status, p.FilesPulled, p.BytesPulled, p.FilesPruned, p.Duration.Round(time.Millisecond))
}

// logAggregateSkill is the gen-skills manifest source for log-aggregate.
func logAggregateSkill() Skill {
	return Skill{
		Name:    "log-aggregate",
		Trigger: "/log-aggregate",
		Summary: "Pull every worker's ~/.hermes/logs/*.jsonl to the gateway over SSH (tar+gzip) and prune old aggregate copies. Wired to com.hermes.logaggregate (daily 03:45).",
		Args: []SkillArg{
			{Name: "--node USER@HOST", Description: "Pull from one worker only (default: every entry in nodes.txt)."},
			{Name: "--keep-days N", Description: "Retention window for files under the landing dir (default 30; negative disables pruning)."},
			{Name: "--json", Description: "Emit one JSON object per pull to stdout."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
			{Name: "--dry-run", Description: "Probe each worker but skip extract/prune/rotate."},
			{Name: "--rotate-on-pull", Description: "Gzip the source *.jsonl files on each worker after a successful pull."},
			{Name: "--timeout DUR", Description: "Per-node SSH connect + tar/extract cap (default 5m)."},
		},
		Examples: []SkillExample{
			{Description: "Default fan-out across the fleet", Command: "fleetctl log-aggregate"},
			{Description: "Targeted pull from one worker", Command: "fleetctl log-aggregate --node hermes@claw1.local"},
			{Description: "Dry-run for capacity planning", Command: "fleetctl log-aggregate --dry-run --json"},
		},
	}
}

func init() {
	registerSkill("log-aggregate", logAggregateSkill)
}
