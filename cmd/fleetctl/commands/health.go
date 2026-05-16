// health.go implements `fleetctl health` — probes gateway LaunchAgents and
// (optionally) one or all SSH workers, then reports a fleet-wide health
// summary in markdown or JSON.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/nodes"
	osh "github.com/owera/owera-fleet/internal/ssh"
	"github.com/owera/owera-fleet/internal/state"
)

var (
	healthNode      string
	healthAll       bool
	healthJSON      bool
	healthQuiet     bool
	healthTimeout   time.Duration
	healthHermesDir string
	healthLogPath   = "~/.hermes/logs/health.jsonl"
)

// HealthCheck is one named check for a node.
type HealthCheck struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Status string `json:"status"` // "ok", "warn", "error", "unknown"
}

// NodeHealth is the health report for one node.
type NodeHealth struct {
	Node   string        `json:"node"`
	Ts     time.Time     `json:"ts"`
	Checks []HealthCheck `json:"checks"`
	Error  string        `json:"error,omitempty"`
}

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Report fleet health: gateway LaunchAgents and (optionally) worker status",
	Long: `health probes the gateway LaunchAgents (always) and optionally SSH workers.

Without --node or --all, only the gateway is probed (no SSH).
--node user@host: probe one worker via SSH.
--all: probe every node in ~/.hermes/nodes.txt via SSH.

Output is a markdown table by default; --json emits a JSON array.
Every probe is appended to ~/.hermes/logs/health.jsonl.`,
	Example: `  fleetctl health
  fleetctl health --all
  fleetctl health --node hermes@claw1.local --json`,
	RunE: runHealth,
}

func init() {
	healthCmd.Flags().StringVar(&healthNode, "node", "", "probe one worker via SSH")
	healthCmd.Flags().BoolVar(&healthAll, "all", false, "probe every entry in ~/.hermes/nodes.txt")
	healthCmd.Flags().BoolVar(&healthJSON, "json", false, "emit JSON array instead of markdown")
	healthCmd.Flags().BoolVar(&healthQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	healthCmd.Flags().DurationVar(&healthTimeout, "timeout", 30*time.Second, "per-node SSH connect+run cap")
	healthCmd.Flags().StringVar(&healthHermesDir, "hermes-dir", "", "override ~/.hermes path")
	rootCmd.AddCommand(healthCmd)
}

// healthStateCollect is overridable in tests.
var healthStateCollect = func(hermesDir string, ctx context.Context) ([]state.LaunchAgent, error) {
	c := state.Default(hermesDir)
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, err
	}
	return snap.LaunchAgents, nil
}

// healthDialer is overridable in tests.
var healthDialer = func(opts ...osh.Option) Dialer {
	return realDialer{d: osh.NewDialer(opts...)}
}

func runHealth(cmd *cobra.Command, _ []string) error {
	if healthNode != "" && healthAll {
		return fmt.Errorf("health: --node and --all are mutually exclusive")
	}

	logSrc := healthLogPath
	if healthHermesDir != "" {
		logSrc = healthHermesDir + "/logs/health.jsonl"
	}
	resolvedLog, err := expandHomePath(logSrc)
	if err != nil {
		return fmt.Errorf("health: expand log path: %w", err)
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("health: open log: %w", err)
	}
	defer logger.Close()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	var results []NodeHealth

	// Always probe gateway.
	gw := probeGateway(ctx, healthHermesDir)
	results = append(results, gw)
	logHealthEvent(logger, gw)

	// Probe workers if requested.
	if healthNode != "" || healthAll {
		targets, err := resolveHealthTargets()
		if err != nil {
			return err
		}
		dialer := healthDialer(osh.WithConnectTimeout(healthTimeout))
		for _, t := range targets {
			nh := probeWorker(ctx, dialer, t)
			results = append(results, nh)
			logHealthEvent(logger, nh)
		}
	}

	if healthQuiet {
		return nil
	}

	if healthJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	renderHealthMarkdown(cmd, results)
	return nil
}

func probeGateway(ctx context.Context, hermesDir string) NodeHealth {
	hostname := "gateway"
	if h, err := hostnameFunc(); err == nil {
		hostname = h
	}
	nh := NodeHealth{Node: "gateway:" + hostname, Ts: time.Now().UTC()}
	agents, err := healthStateCollect(hermesDir, ctx)
	if err != nil {
		nh.Error = err.Error()
		nh.Checks = append(nh.Checks, HealthCheck{Name: "state", Value: err.Error(), Status: "error"})
		return nh
	}
	for _, a := range agents {
		status := "stopped"
		if a.PID != "" && a.PID != "-" && a.PID != "0" {
			status = "running"
		}
		nh.Checks = append(nh.Checks, HealthCheck{
			Name:   a.Label,
			Value:  fmt.Sprintf("pid=%s exit=%s", a.PID, a.LastExit),
			Status: status,
		})
	}
	if len(nh.Checks) == 0 {
		nh.Checks = append(nh.Checks, HealthCheck{Name: "launchd", Value: "no com.hermes.* services", Status: "warn"})
	}
	return nh
}

// hostnameFunc is overridable in tests.
var hostnameFunc func() (string, error) = os.Hostname

func probeWorker(ctx context.Context, dialer Dialer, target osh.Target) NodeHealth {
	nodeLabel := target.User + "@" + target.Host
	nh := NodeHealth{Node: nodeLabel, Ts: time.Now().UTC()}

	dialCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	client, err := dialer.Dial(dialCtx, target)
	if err != nil {
		nh.Error = err.Error()
		nh.Checks = append(nh.Checks, HealthCheck{Name: "ssh", Value: err.Error(), Status: "error"})
		return nh
	}
	defer client.Close()

	type probe struct {
		name string
		cmd  string
	}
	probes := []probe{
		{"os", "sw_vers -productVersion 2>/dev/null || echo unknown"},
		{"uptime", "uptime | sed 's/.*up /up /'"},
		{"disk", "df -h / | awk 'NR==2{print $5\" used of \"$2}'"},
		{"services", "launchctl list 2>/dev/null | awk '/com.hermes/{print $3}' | tr '\n' ' '"},
	}
	for _, p := range probes {
		res, runErr := client.Run(dialCtx, p.cmd)
		val := strings.TrimSpace(res.Stdout)
		if runErr != nil || res.ExitCode != 0 {
			nh.Checks = append(nh.Checks, HealthCheck{Name: p.name, Value: val, Status: "warn"})
		} else {
			nh.Checks = append(nh.Checks, HealthCheck{Name: p.name, Value: val, Status: "ok"})
		}
	}
	return nh
}

func resolveHealthTargets() ([]osh.Target, error) {
	if healthNode != "" {
		t, err := osh.ParseTarget(healthNode)
		if err != nil {
			return nil, fmt.Errorf("health: parse --node: %w", err)
		}
		return []osh.Target{t}, nil
	}
	reg, err := nodes.Load("")
	if err != nil {
		return nil, fmt.Errorf("health: load nodes: %w", err)
	}
	all := reg.All()
	out := make([]osh.Target, 0, len(all))
	for _, n := range all {
		t, err := osh.ParseTarget(n.String())
		if err != nil {
			return nil, fmt.Errorf("health: parse node %s: %w", n, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func logHealthEvent(logger *log.Logger, nh NodeHealth) {
	if logger == nil {
		return
	}
	result := log.ResultOK
	tail := ""
	if nh.Error != "" {
		result = log.ResultError
		tail = nh.Error
	} else {
		for _, c := range nh.Checks {
			if c.Status == "error" {
				result = log.ResultError
				break
			}
			if c.Status == "warn" && result == log.ResultOK {
				result = log.ResultWarn
			}
		}
	}
	_ = logger.Action(log.Event{
		Node:       nh.Node,
		Phase:      "health",
		Action:     "probe:" + nh.Node,
		Result:     result,
		DurationMs: 0,
		StderrTail: tail,
	})
}

func renderHealthMarkdown(cmd *cobra.Command, results []NodeHealth) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "# Fleet Health — %s\n\n", time.Now().UTC().Format(time.RFC3339))
	for _, nh := range results {
		fmt.Fprintf(w, "## %s\n\n", nh.Node)
		if nh.Error != "" {
			fmt.Fprintf(w, "**Error:** %s\n\n", nh.Error)
			continue
		}
		fmt.Fprintf(w, "| Check | Value | Status |\n|---|---|---|\n")
		for _, c := range nh.Checks {
			fmt.Fprintf(w, "| %s | %s | %s |\n", c.Name, c.Value, c.Status)
		}
		fmt.Fprintln(w)
	}
}

func healthSkill() Skill {
	return Skill{
		Name:    "health",
		Trigger: "/health",
		Summary: "Report fleet health: gateway LaunchAgents and SSH worker checks.",
		Args: []SkillArg{
			{Name: "--node USER@HOST", Description: "Probe one worker via SSH."},
			{Name: "--all", Description: "Probe every entry in ~/.hermes/nodes.txt."},
			{Name: "--json", Description: "Emit JSON array instead of markdown."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
			{Name: "--timeout DUR", Description: "Per-node SSH connect+run cap (default 30s)."},
			{Name: "--hermes-dir PATH", Description: "Override ~/.hermes path."},
		},
		Examples: []SkillExample{
			{Description: "Gateway-only health check", Command: "fleetctl health"},
			{Description: "All workers health", Command: "fleetctl health --all"},
			{Description: "One worker in JSON", Command: "fleetctl health --node hermes@claw1.local --json"},
		},
	}
}

func init() { registerSkill("health", healthSkill) }
