// delegate.go implements `fleetctl delegate` — runs an ad-hoc shell command
// on a single worker, a randomly-chosen worker, or every worker in
// ~/.hermes/nodes.txt, and emits one JSONL record per invocation to
// ~/.hermes/logs/delegate.jsonl.
//
// This is the typed successor to hermes-setup's delegate-to-node.sh — the
// most-used script in the bash codebase. The JSONL schema matches the
// canonical {ts,node,phase,action,result,duration_ms,stderr_tail} shape
// emitted by every other fleetctl subcommand via [internal/log], so the
// audit roll-up (`fleetctl state`) consumes delegate output without a
// special case.
//
// Differences from the bash original:
//
//   - `action` is the first 80 chars of the dispatched command (the bash
//     script wrote the literal string "run"). The richer value lets the
//     audit log answer "what was actually delegated?" without a join.
//   - `result` uses [log.ResultError] instead of "fail" so the field
//     stays in the canonical [log.Result] vocabulary.
//   - The dialer is injected through a local Dialer interface, so tests
//     never hit the live fleet.
package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

// delegateLogPath is the canonical JSONL log location, mirroring the bash
// original. Overridable in tests via the package-level variable.
var delegateLogPath = "~/.hermes/logs/delegate.jsonl"

// delegateNodesPath defaults to nodes.Load(""), which resolves to
// ~/.hermes/nodes.txt. Tests override this with a fixture path.
var delegateNodesPath = ""

// delegateActionMax is the cap on the command prefix recorded into the
// `action` log field. 80 chars keeps lines visually scannable; the full
// command is recoverable from the operator's shell history if needed.
const delegateActionMax = 80

var (
	delegateNode     string
	delegateAll      bool
	delegateRandom   bool
	delegateCmd      string
	delegateTimeout  time.Duration
	delegateJSON     bool
	delegateQuiet    bool
	delegateAnyNode  bool
	delegateTenantID string
)

var delegateCobraCmd = &cobra.Command{
	Use:   "delegate",
	Short: "Run an ad-hoc shell command on one, all, or a random worker",
	Long: `delegate dispatches a shell command to worker(s) and records one JSONL
event per target into ~/.hermes/logs/delegate.jsonl.

Exactly one of --node, --all, or --random must be supplied. --cmd is
required. By default the targeted node must appear in ~/.hermes/nodes.txt;
pass --any-node to allow an off-registry --node target (the safety net
prevents typo'd hostnames from silently fanning out to dev machines).

Exit code: 0 only if every selected node returned 0; non-zero otherwise.`,
	Example: `  fleetctl delegate --node hermes@claw1.local --cmd "uname -a"
  fleetctl delegate --all --cmd "df -h /"
  fleetctl delegate --random --cmd "uptime" --json`,
	RunE: runDelegate,
}

func init() {
	delegateCobraCmd.Flags().StringVar(&delegateNode, "node", "", "target user@host (mutually exclusive with --all/--random)")
	delegateCobraCmd.Flags().BoolVar(&delegateAll, "all", false, "fan out to every entry in ~/.hermes/nodes.txt")
	delegateCobraCmd.Flags().BoolVar(&delegateRandom, "random", false, "pick one node uniformly at random")
	delegateCobraCmd.Flags().StringVar(&delegateCmd, "cmd", "", "remote shell command (required)")
	delegateCobraCmd.Flags().DurationVar(&delegateTimeout, "timeout", 60*time.Second, "per-node ssh connect + run cap")
	delegateCobraCmd.Flags().BoolVar(&delegateJSON, "json", false, "emit one JSON object per target to stdout")
	delegateCobraCmd.Flags().BoolVar(&delegateQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	delegateCobraCmd.Flags().BoolVar(&delegateAnyNode, "any-node", false, "allow --node for a target not in nodes.txt")
	// --tenant-id is accepted (and recorded into the action prefix) so
	// operators can exercise the issue-#5 customer-plane tenant pipeline
	// against delegate's ad-hoc command path. Delegate itself does not
	// write ledger entries; the flag is informational here and is the
	// primary tenant-flow integration point on `fleetctl swarm`.
	delegateCobraCmd.Flags().StringVar(&delegateTenantID, "tenant-id", "", "tenant ID stamped into the audit action (informational on delegate)")
	rootCmd.AddCommand(delegateCobraCmd)
}

// Dialer is the subset of *ssh.Dialer that delegate consumes. Declared
// locally so tests can substitute a fake without touching internal/ssh.
type Dialer interface {
	Dial(ctx context.Context, t osh.Target, opts ...osh.Option) (RemoteClient, error)
}

// RemoteClient is the subset of *ssh.Client that delegate consumes.
type RemoteClient interface {
	Run(ctx context.Context, cmd string) (osh.Result, error)
	Close() error
}

// realDialer adapts *ssh.Dialer to the local Dialer interface (the SSH
// package returns its own concrete *Client, which is structurally
// compatible with RemoteClient but not interface-typed).
type realDialer struct{ d *osh.Dialer }

func (r realDialer) Dial(ctx context.Context, t osh.Target, opts ...osh.Option) (RemoteClient, error) {
	c, err := r.d.Dial(ctx, t, opts...)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// newDialer is overridable in tests; production wraps ssh.NewDialer.
var newDialer = func(opts ...osh.Option) Dialer {
	return realDialer{d: osh.NewDialer(opts...)}
}

// openLogger is overridable in tests; production wraps log.Open.
var openLogger = func(path string) (*log.Logger, error) { return log.Open(path) }

func runDelegate(cmd *cobra.Command, _ []string) error {
	if err := validateDelegateFlags(); err != nil {
		return err
	}

	reg, err := nodes.Load(delegateNodesPath)
	if err != nil && !(delegateAnyNode && delegateNode != "") {
		// --any-node + --node lets us bypass the registry entirely, which
		// is the documented escape hatch for a fresh gateway that hasn't
		// populated nodes.txt yet.
		return fmt.Errorf("delegate: load nodes: %w", err)
	}

	targets, err := resolveDelegateTargets(reg)
	if err != nil {
		return err
	}

	resolvedLogPath, err := expandHomePath(delegateLogPath)
	if err != nil {
		return fmt.Errorf("delegate: expand log path: %w", err)
	}
	logger, err := openLogger(resolvedLogPath)
	if err != nil {
		return fmt.Errorf("delegate: open log: %w", err)
	}
	defer logger.Close()

	dialer := newDialer(osh.WithConnectTimeout(delegateTimeout))

	// cmd.Context() may be nil when called outside Execute (e.g. tests
	// that drive RunE directly). Fall back to a background context so
	// the WithTimeout call has a parent.
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	overall := 0
	for _, t := range targets {
		exit := dispatchOne(parent, cmd.OutOrStdout(), cmd.ErrOrStderr(), dialer, logger, t)
		if exit != 0 && overall == 0 {
			overall = exit
		}
	}
	if overall != 0 {
		// Returning an error here would print a noisy cobra "Error:" wrapper
		// over the per-node detail already streamed to the operator. Use
		// SilenceErrors + an explicit non-zero exit code instead.
		cmd.SilenceUsage = true
		return fmt.Errorf("delegate: one or more targets failed")
	}
	return nil
}

// validateDelegateFlags enforces the mutual-exclusion + required-flag
// invariants. Returns user-facing errors so cobra surfaces them verbatim.
func validateDelegateFlags() error {
	modes := 0
	if delegateNode != "" {
		modes++
	}
	if delegateAll {
		modes++
	}
	if delegateRandom {
		modes++
	}
	if modes == 0 {
		return errors.New("delegate: must specify exactly one of --node, --all, or --random")
	}
	if modes > 1 {
		return errors.New("delegate: --node, --all, and --random are mutually exclusive")
	}
	if strings.TrimSpace(delegateCmd) == "" {
		return errors.New("delegate: --cmd is required")
	}
	return nil
}

// resolveDelegateTargets turns the three target-selection flags plus the
// loaded registry into the ordered list of [ssh.Target]s to dispatch to.
func resolveDelegateTargets(reg *nodes.Registry) ([]osh.Target, error) {
	switch {
	case delegateNode != "":
		if !delegateAnyNode {
			if reg == nil {
				return nil, fmt.Errorf("delegate: node %s not in registry (pass --any-node to override)", delegateNode)
			}
			if _, err := reg.One(delegateNode); err != nil {
				return nil, fmt.Errorf("delegate: node %s not in nodes.txt (pass --any-node to override)", delegateNode)
			}
		}
		t, err := osh.ParseTarget(delegateNode)
		if err != nil {
			return nil, err
		}
		return []osh.Target{t}, nil
	case delegateAll:
		if reg == nil || reg.Len() == 0 {
			return nil, errors.New("delegate: --all requested but nodes.txt is empty")
		}
		all := reg.All()
		out := make([]osh.Target, 0, len(all))
		for _, n := range all {
			t, err := osh.ParseTarget(n.String())
			if err != nil {
				return nil, fmt.Errorf("delegate: parse %s: %w", n, err)
			}
			out = append(out, t)
		}
		return out, nil
	case delegateRandom:
		if reg == nil || reg.Len() == 0 {
			return nil, errors.New("delegate: --random requested but nodes.txt is empty")
		}
		n, err := reg.Random()
		if err != nil {
			return nil, err
		}
		t, err := osh.ParseTarget(n.String())
		if err != nil {
			return nil, err
		}
		return []osh.Target{t}, nil
	}
	return nil, errors.New("delegate: unreachable target selection")
}

// dispatchOne runs the command against a single target, writes its JSONL
// event, and streams the human / --json output to stdout. Returns 0 on
// success, non-zero on any failure (dial, run, or non-zero exit on the
// remote).
func dispatchOne(ctx context.Context, stdout io.Writer, stderr io.Writer, dialer Dialer, logger *log.Logger, target osh.Target) int {
	action := truncateAction(delegateCmd)
	if delegateTenantID != "" {
		// Prefix the action so downstream audit tooling can filter by
		// `tenant=<id>` without a separate JSONL field. internal/log.Event
		// does not (yet) carry a tenant column; this keeps the
		// customer-plane audit signal visible without expanding the
		// log schema in a separate package.
		action = "tenant=" + delegateTenantID + " " + action
		if len(action) > delegateActionMax {
			action = action[:delegateActionMax]
		}
	}
	// nodeLabel is the bare user@host form (no port) so the JSONL stream
	// stays compatible with hermes-setup's existing delegate.jsonl shape
	// and any downstream `jq` audit pipelines keyed on a port-less host.
	nodeLabel := target.User + "@" + target.Host
	start := time.Now()

	dialCtx, cancel := context.WithTimeout(ctx, delegateTimeout)
	defer cancel()

	client, err := dialer.Dial(dialCtx, target, osh.WithConnectTimeout(delegateTimeout))
	if err != nil {
		dur := time.Since(start).Milliseconds()
		_ = logger.Action(log.Event{
			Node:       nodeLabel,
			Phase:      "delegate",
			Action:     action,
			Result:     log.ResultError,
			DurationMs: dur,
			StderrTail: err.Error(),
		})
		writeDelegateOutput(stdout, stderr, target, "", err.Error(), -1, dur)
		return 1
	}
	defer client.Close()

	res, runErr := client.Run(dialCtx, delegateCmd)
	dur := time.Since(start).Milliseconds()

	result := log.ResultOK
	if runErr != nil {
		result = log.ResultError
	} else if res.ExitCode != 0 {
		result = log.ResultError
	}

	stderrTail := tailBytes(res.Stderr, 4096)
	if runErr != nil {
		// Preserve the transport error in the tail when the SSH layer
		// itself failed (e.g. session signalled, ctx cancelled).
		if stderrTail == "" {
			stderrTail = runErr.Error()
		} else {
			stderrTail = stderrTail + "\n" + runErr.Error()
			stderrTail = tailBytes(stderrTail, 4096)
		}
	}

	_ = logger.Action(log.Event{
		Node:       nodeLabel,
		Phase:      "delegate",
		Action:     action,
		Result:     result,
		DurationMs: dur,
		StderrTail: stderrTail,
	})

	writeDelegateOutput(stdout, stderr, target, res.Stdout, res.Stderr, res.ExitCode, dur)
	if result != log.ResultOK {
		return 1
	}
	return 0
}

// writeDelegateOutput handles the operator-facing stdout/stderr split:
// --quiet suppresses it entirely, --json prints one structured line per
// target, otherwise the human-readable banner + body.
func writeDelegateOutput(stdout, stderr io.Writer, target osh.Target, stdoutContent, stderrContent string, exit int, dur int64) {
	if delegateQuiet {
		return
	}
	if delegateJSON {
		// Build the JSON manually rather than encoding/json to keep the
		// shape identical to the bash --json shape and to avoid a struct
		// for one local emission.
		fmt.Fprintf(stdout, `{"node":%q,"exit":%d,"duration_ms":%d,"stdout":%q,"stderr":%q}`+"\n",
			target.String(), exit, dur, stdoutContent, stderrContent)
		return
	}
	fmt.Fprintf(stderr, "\n=== %s (exit=%d, %dms) ===\n", target.String(), exit, dur)
	if stdoutContent != "" {
		fmt.Fprint(stdout, stdoutContent)
		if !strings.HasSuffix(stdoutContent, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	if stderrContent != "" {
		fmt.Fprint(stderr, stderrContent)
		if !strings.HasSuffix(stderrContent, "\n") {
			fmt.Fprintln(stderr)
		}
	}
}

// truncateAction returns the first delegateActionMax chars of cmd with
// newlines/tabs flattened so the JSONL `action` field stays single-line.
func truncateAction(cmd string) string {
	flat := strings.ReplaceAll(cmd, "\n", " ")
	flat = strings.ReplaceAll(flat, "\t", " ")
	flat = strings.TrimSpace(flat)
	if len(flat) > delegateActionMax {
		flat = flat[:delegateActionMax]
	}
	return flat
}

// tailBytes returns the last n bytes of s. UTF-8 boundaries are not
// preserved — stderr is treated as bytes since stack traces and binary
// blobs both end up here.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// expandHomePath expands a leading "~/" against $HOME. Kept local rather
// than reaching into internal/ssh's expandHome (unexported) or
// internal/nodes' expandHome (also unexported).
func expandHomePath(p string) (string, error) {
	if !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return home + p[1:], nil
}

// userHomeDir is overridable in tests; production wraps os.UserHomeDir.
var userHomeDir = os.UserHomeDir

func delegateSkill() Skill {
	return Skill{
		Name:    "delegate",
		Trigger: "/delegate",
		Summary: "Run an ad-hoc shell command on one, all, or a random worker, and append a JSONL audit event per target.",
		Args: []SkillArg{
			{Name: "--node USER@HOST", Description: "Target a single worker (mutually exclusive with --all/--random)."},
			{Name: "--all", Description: "Fan out to every entry in ~/.hermes/nodes.txt."},
			{Name: "--random", Description: "Pick one node uniformly at random."},
			{Name: "--cmd \"<string>\"", Description: "Remote shell command (required)."},
			{Name: "--timeout DUR", Description: "Per-node ssh connect + run cap (default 60s)."},
			{Name: "--json", Description: "Emit one JSON object per target to stdout."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log is still written."},
			{Name: "--any-node", Description: "Allow --node for a target not in nodes.txt."},
			{Name: "--tenant-id ID", Description: "Tenant ID stamped into the audit action prefix (informational on delegate)."},
		},
		Examples: []SkillExample{
			{Description: "Run a probe on one worker", Command: `fleetctl delegate --node hermes@claw1.local --cmd "uname -a"`},
			{Description: "Disk check across the fleet", Command: `fleetctl delegate --all --cmd "df -h /"`},
			{Description: "JSON output for piping", Command: `fleetctl delegate --random --cmd "uptime" --json`},
		},
	}
}

func init() { registerSkill("delegate", delegateSkill) }
