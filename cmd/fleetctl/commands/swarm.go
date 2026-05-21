// swarm.go implements `fleetctl swarm` — orchestrator + leaf-subagent fan-out
// across worker nodes, with per-leaf ledger entries merged into the parent
// task ledger. Used for §3.2 marketing fan-out and similar campaigns.
package commands

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/ledger"
	osh "github.com/owera/owera-fleet/internal/ssh"
	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
	"github.com/owera/owera-fleet/internal/orchestrator"
)

var (
	swarmTaskID    string
	swarmTenantID  string
	swarmParentRun string
	swarmCmdStr    string
	swarmTargets   []string
	swarmAll       bool
	swarmParallel  int
	swarmRetry     int
	swarmLedgerDir string
	swarmTimeout   time.Duration
	swarmJSON      bool
	swarmDryRun    bool
	swarmLogPath   = "~/.hermes/logs/swarm.jsonl"
)

var swarmCmd = &cobra.Command{
	Use:   "swarm",
	Short: "Run an orchestrator + N leaf-subagent swarm",
	Long: `swarm dispatches a per-leaf command across multiple worker nodes,
merges the per-worker ledger entries into the parent task ledger, and
returns a unified campaign report.

Each leaf runs the same --cmd on its assigned node. Use --target multiple
times to nominate specific nodes, or --all to fan out to every node in
nodes.txt.`,
	Example: `  fleetctl swarm --cmd "hermes z 'post launch tweet'" --all
  fleetctl swarm --cmd "uptime" --target claw1.local --target claw2.local --parallel 2`,
	RunE: runSwarm,
}

func init() {
	swarmCmd.Flags().StringVar(&swarmTaskID, "task-id", "", "parent task ID (generated if empty)")
	swarmCmd.Flags().StringVar(&swarmTenantID, "tenant-id", "", "customer-plane tenant stamped onto every ledger entry (empty = operator-only)")
	swarmCmd.Flags().StringVar(&swarmParentRun, "parent-run", "swarm", "free-form description of the swarm run")
	swarmCmd.Flags().StringVar(&swarmCmdStr, "cmd", "", "command each leaf runs on its node (required)")
	swarmCmd.Flags().StringSliceVar(&swarmTargets, "target", nil, "specific node to include (repeatable)")
	swarmCmd.Flags().BoolVar(&swarmAll, "all", false, "fan out to every node in nodes.txt")
	swarmCmd.Flags().IntVar(&swarmParallel, "parallel", 0, "max concurrent leaves (0 = unbounded)")
	swarmCmd.Flags().IntVar(&swarmRetry, "retry", 0, "per-leaf retries on transient failure")
	swarmCmd.Flags().StringVar(&swarmLedgerDir, "ledger-dir", "~/.hermes/ledger", "ledger directory")
	swarmCmd.Flags().DurationVar(&swarmTimeout, "timeout", 5*time.Minute, "per-leaf timeout")
	swarmCmd.Flags().BoolVar(&swarmJSON, "json", false, "emit JSON report")
	swarmCmd.Flags().BoolVar(&swarmDryRun, "dry-run", false, "resolve the leaf plan and print it without dialing SSH")
	rootCmd.AddCommand(swarmCmd)
}

// swarmDialer is overridable in tests.
var swarmDialer = func(opts ...osh.Option) Dialer { return realDialer{d: osh.NewDialer(opts...)} }

func runSwarm(cmd *cobra.Command, _ []string) error {
	if swarmCmdStr == "" {
		return errors.New("swarm: --cmd is required")
	}
	if !swarmAll && len(swarmTargets) == 0 {
		return errors.New("swarm: --all or one or more --target are required")
	}

	resolvedLog, err := expandHomePath(swarmLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return err
	}
	defer logger.Close()

	ledgerDir, err := expandHomePath(swarmLedgerDir)
	if err != nil {
		return err
	}
	led, err := ledger.Open(ledgerDir)
	if err != nil {
		return err
	}

	registry, err := nodes.Load("")
	if err != nil {
		return fmt.Errorf("swarm: load nodes: %w", err)
	}

	var targets []nodes.Node
	if swarmAll {
		targets = registry.All()
	} else {
		for _, name := range swarmTargets {
			n, err := registry.One(name)
			if err != nil {
				return fmt.Errorf("swarm: target %q: %w", name, err)
			}
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		return errors.New("swarm: no targets resolved")
	}

	taskID := swarmTaskID
	if taskID == "" {
		taskID = genTaskID()
	}

	plan := orchestrator.Plan{
		TaskID:      taskID,
		TenantID:    swarmTenantID,
		ParentRun:   swarmParentRun,
		MaxParallel: swarmParallel,
		RetryEach:   swarmRetry,
	}
	for i, t := range targets {
		plan.Leaves = append(plan.Leaves, orchestrator.LeafInput{
			Node:    t.String(),
			TaskID:  taskID,
			LeafID:  fmt.Sprintf("leaf-%02d", i+1),
			Payload: map[string]any{"cmd": swarmCmdStr, "host": t.Host, "user": t.User},
		})
	}

	if swarmDryRun {
		w := cmd.OutOrStdout()
		if swarmJSON {
			return json.NewEncoder(w).Encode(plan)
		}
		fmt.Fprintf(w, "DRY-RUN: would dispatch task=%s parent-run=%s parallel=%d retry=%d timeout=%s\n",
			plan.TaskID, plan.ParentRun, plan.MaxParallel, plan.RetryEach, swarmTimeout)
		for _, leaf := range plan.Leaves {
			fmt.Fprintf(w, "  %s -> %s: %s\n", leaf.LeafID, leaf.Node, swarmCmdStr)
		}
		return nil
	}

	o := &orchestrator.Orchestrator{
		Ledger: led,
		Run:    makeSwarmRunner(swarmTimeout),
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := o.Execute(ctx, plan)
	if err != nil {
		return err
	}

	_ = logger.Action(log.Event{
		Node:   "gateway",
		Phase:  "swarm",
		Action: "task:" + taskID,
		Result: swarmResultStatus(res.OK),
		DurationMs: res.EndedAt.Sub(res.StartedAt).Milliseconds(),
	})

	if swarmJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	}

	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%s\n", res.Summary())
	for _, l := range res.Leaves {
		status := "ok"
		extra := ""
		if l.Err != nil {
			status = "ERR"
			extra = "  err=" + l.Err.Error()
		}
		fmt.Fprintf(w, "  %s [%s] %s%s\n", l.LeafID, l.Node, status, extra)
	}
	if !res.OK {
		return errors.New("swarm: one or more leaves failed")
	}
	return nil
}

// makeSwarmRunner returns a LeafRunner that SSHes into each target and runs
// the per-leaf payload command.
func makeSwarmRunner(timeout time.Duration) orchestrator.LeafRunner {
	return func(ctx context.Context, in orchestrator.LeafInput) ([]ledger.Entry, error) {
		cmdStr, _ := in.Payload["cmd"].(string)
		user, _ := in.Payload["user"].(string)
		host, _ := in.Payload["host"].(string)
		target := osh.Target{User: user, Host: host}

		dialer := swarmDialer(osh.WithConnectTimeout(timeout))
		client, err := dialer.Dial(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", target, err)
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		res, err := client.Run(runCtx, cmdStr)
		durMs := time.Since(start).Milliseconds()
		if err != nil {
			return []ledger.Entry{{
				Phase:      "leaf",
				Action:     "ssh:" + target.String(),
				Result:     ledger.ResultError,
				DurationMs: durMs,
				StderrTail: truncateAction(err.Error()),
			}}, err
		}
		entry := ledger.Entry{
			Phase:      "leaf",
			Action:     "ssh:" + target.String(),
			Result:     ledger.ResultOK,
			DurationMs: durMs,
			// Capture leaf stdout so scatter-gather workloads (e.g. JSON
			// benchmark lines) can be reassembled from the parent ledger
			// rather than discarded. Truncation matches the StderrTail policy.
			StdoutTail: truncateAction(res.Stdout),
		}
		if res.ExitCode != 0 {
			entry.Result = ledger.ResultError
			entry.StderrTail = truncateAction(res.Stderr)
		}
		return []ledger.Entry{entry}, nil
	}
}

func swarmResultStatus(ok bool) log.Result {
	if ok {
		return log.ResultOK
	}
	return log.ResultError
}

func genTaskID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "task-" + strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

func swarmSkill() Skill {
	return Skill{
		Name:    "swarm",
		Trigger: "/swarm",
		Summary: "Run an orchestrator + N leaf-subagent swarm with per-leaf ledger merge.",
		Args: []SkillArg{
			{Name: "--cmd CMD", Description: "Per-leaf command (required)."},
			{Name: "--all", Description: "Fan out to every node in nodes.txt."},
			{Name: "--target N", Description: "Specific node (repeatable)."},
			{Name: "--parallel N", Description: "Max concurrent leaves (0 = unbounded)."},
			{Name: "--retry N", Description: "Per-leaf retries on transient failure."},
			{Name: "--task-id ID", Description: "Override parent task ID."},
			{Name: "--tenant-id ID", Description: "Customer-plane tenant stamped onto every ledger entry (empty = operator-only)."},
			{Name: "--timeout DUR", Description: "Per-leaf timeout (default 5m)."},
			{Name: "--json", Description: "Emit JSON report."},
			{Name: "--dry-run", Description: "Resolve the leaf plan and print it without dialing SSH."},
		},
		Examples: []SkillExample{
			{Description: "Marketing fan-out", Command: "fleetctl swarm --cmd \"hermes z 'post launch tweet'\" --all"},
			{Description: "Multi-target uptime", Command: "fleetctl swarm --cmd uptime --target claw1.local --target claw2.local --parallel 2"},
			{Description: "Preview the plan", Command: "fleetctl swarm --cmd uptime --all --dry-run"},
		},
	}
}

func init() { registerSkill("swarm", swarmSkill) }
