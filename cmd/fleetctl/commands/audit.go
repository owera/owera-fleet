// audit.go implements `fleetctl audit` — reads one or more JSONL log files
// from ~/.hermes/logs/ and produces a summary report of fleet actions.
package commands

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
)

var (
	auditLog       []string
	auditSince     string
	auditResult    string
	auditPhase     string
	auditNode      string
	auditJSON      bool
	auditQuiet     bool
	auditHermesDir string
	auditLogDir    = "~/.hermes/logs"
)

// AuditEvent is a parsed log event for rendering.
type AuditEvent struct {
	log.Event
	Source string `json:"source"` // log file basename
}

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Summarise fleet JSONL audit logs",
	Long: `audit reads JSONL event files from ~/.hermes/logs/ and renders a
filtered, sorted summary table.

Without --log, all *.jsonl files in the logs directory are scanned.
Use --since, --phase, --node, or --result to narrow the output.`,
	Example: `  fleetctl audit
  fleetctl audit --phase delegate --result error
  fleetctl audit --log delegate.jsonl --since 2026-05-01
  fleetctl audit --json`,
	RunE: runAudit,
}

func init() {
	auditCmd.Flags().StringSliceVar(&auditLog, "log", nil, "specific log file(s) to scan (basename or full path)")
	auditCmd.Flags().StringVar(&auditSince, "since", "", "only events at or after this time (RFC3339 or YYYY-MM-DD)")
	auditCmd.Flags().StringVar(&auditResult, "result", "", "filter by result: ok, error, warn, no-change, skipped")
	auditCmd.Flags().StringVar(&auditPhase, "phase", "", "filter by phase")
	auditCmd.Flags().StringVar(&auditNode, "node", "", "filter by node (substring match)")
	auditCmd.Flags().BoolVar(&auditJSON, "json", false, "emit JSON array of matching events")
	auditCmd.Flags().BoolVar(&auditQuiet, "quiet", false, "suppress output (useful for exit-code checks)")
	auditCmd.Flags().StringVar(&auditHermesDir, "hermes-dir", "", "override ~/.hermes path")
	rootCmd.AddCommand(auditCmd)
}

func runAudit(cmd *cobra.Command, _ []string) error {
	logDir := auditLogDir
	if auditHermesDir != "" {
		logDir = auditHermesDir + "/logs"
	}
	resolvedDir, err := expandHomePath(logDir)
	if err != nil {
		return fmt.Errorf("audit: expand log dir: %w", err)
	}

	// Determine which files to scan.
	var paths []string
	if len(auditLog) > 0 {
		for _, l := range auditLog {
			if filepath.IsAbs(l) {
				paths = append(paths, l)
			} else {
				paths = append(paths, filepath.Join(resolvedDir, l))
			}
		}
	} else {
		entries, err := os.ReadDir(resolvedDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("audit: log directory %s does not exist", resolvedDir)
			}
			return fmt.Errorf("audit: read dir %s: %w", resolvedDir, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				paths = append(paths, filepath.Join(resolvedDir, e.Name()))
			}
		}
		sort.Strings(paths)
	}

	if len(paths) == 0 {
		return fmt.Errorf("audit: no JSONL files found in %s", resolvedDir)
	}

	// Parse time filter.
	var since time.Time
	if auditSince != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			t, err := time.Parse(layout, auditSince)
			if err == nil {
				since = t
				break
			}
		}
		if since.IsZero() {
			return fmt.Errorf("audit: --since %q is not RFC3339 or YYYY-MM-DD", auditSince)
		}
	}

	// Read and filter events.
	var events []AuditEvent
	for _, p := range paths {
		evs, err := readJSONL(p)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "audit: skip %s: %v\n", p, err)
			continue
		}
		source := filepath.Base(p)
		for _, ev := range evs {
			if !since.IsZero() && ev.Timestamp.Before(since) {
				continue
			}
			if auditResult != "" && string(ev.Result) != auditResult {
				continue
			}
			if auditPhase != "" && ev.Phase != auditPhase {
				continue
			}
			if auditNode != "" && !strings.Contains(ev.Node, auditNode) {
				continue
			}
			events = append(events, AuditEvent{Event: ev, Source: source})
		}
	}

	if auditQuiet {
		if len(events) == 0 {
			return errors.New("audit: no matching events")
		}
		return nil
	}

	if auditJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(events)
	}

	renderAuditTable(cmd, events)
	return nil
}

func readJSONL(path string) ([]log.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []log.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev log.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // skip malformed lines
		}
		events = append(events, ev)
	}
	return events, sc.Err()
}

func renderAuditTable(cmd *cobra.Command, events []AuditEvent) {
	w := cmd.OutOrStdout()
	if len(events) == 0 {
		fmt.Fprintln(w, "(no matching events)")
		return
	}
	fmt.Fprintf(w, "%-24s %-16s %-12s %-30s %-10s %s\n",
		"Timestamp", "Node", "Phase", "Action", "Result", "Source")
	fmt.Fprintln(w, strings.Repeat("-", 110))
	for _, ev := range events {
		ts := ev.Timestamp.Format("2006-01-02T15:04:05Z")
		node := ev.Node
		if len(node) > 16 {
			node = node[:13] + "..."
		}
		action := ev.Action
		if len(action) > 30 {
			action = action[:27] + "..."
		}
		fmt.Fprintf(w, "%-24s %-16s %-12s %-30s %-10s %s\n",
			ts, node, ev.Phase, action, string(ev.Result), ev.Source)
	}
	fmt.Fprintf(w, "\n%d event(s)\n", len(events))
}

func auditSkill() Skill {
	return Skill{
		Name:    "audit",
		Trigger: "/audit",
		Summary: "Summarise fleet JSONL audit logs with filtering by phase, node, result, and time.",
		Args: []SkillArg{
			{Name: "--log FILE", Description: "Specific JSONL file(s) to scan (default: all in ~/.hermes/logs/)."},
			{Name: "--since DATE", Description: "Only events at or after this time (RFC3339 or YYYY-MM-DD)."},
			{Name: "--phase PHASE", Description: "Filter by phase (delegate, health, bootstrap, …)."},
			{Name: "--node NODE", Description: "Filter by node (substring match)."},
			{Name: "--result RESULT", Description: "Filter by result: ok, error, warn, no-change, skipped."},
			{Name: "--json", Description: "Emit JSON array of matching events."},
			{Name: "--hermes-dir PATH", Description: "Override ~/.hermes path."},
		},
		Examples: []SkillExample{
			{Description: "Show all recent events", Command: "fleetctl audit"},
			{Description: "Show only errors", Command: "fleetctl audit --result error"},
			{Description: "Delegate events since May 2026", Command: "fleetctl audit --phase delegate --since 2026-05-01"},
		},
	}
}

func init() { registerSkill("audit", auditSkill) }
