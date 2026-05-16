// ledger_cmd.go implements `fleetctl ledger` subcommands for inspecting and
// verifying the signed per-task ledger stored under ~/.hermes/ledger/.
package commands

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	iledger "github.com/owera/owera-fleet/internal/ledger"
)

var (
	ledgerDir    string
	ledgerTaskID string
	ledgerJSON   bool
)

var ledgerCmd = &cobra.Command{
	Use:   "ledger",
	Short: "Inspect and verify the signed per-task ledger",
	Long: `ledger provides subcommands to list tasks, show entries, and verify
ed25519 signatures in the fleet's per-task signed JSONL ledger stored
under ~/.hermes/ledger/.`,
}

var ledgerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List task IDs that have ledger entries",
	RunE:  runLedgerList,
}

var ledgerShowCmd = &cobra.Command{
	Use:   "show <task-id>",
	Short: "Show signed entries for a task",
	Args:  cobra.ExactArgs(1),
	RunE:  runLedgerShow,
}

var ledgerVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify signatures across all ledger tasks",
	RunE:  runLedgerVerify,
}

func init() {
	ledgerCmd.PersistentFlags().StringVar(&ledgerDir, "ledger-dir", "~/.hermes/ledger", "ledger directory")
	ledgerCmd.PersistentFlags().BoolVar(&ledgerJSON, "json", false, "emit JSON output")
	ledgerCmd.AddCommand(ledgerListCmd, ledgerShowCmd, ledgerVerifyCmd)
	ledgerShowCmd.Flags().StringVar(&ledgerTaskID, "task", "", "task ID (overrides positional arg)")
	rootCmd.AddCommand(ledgerCmd)
}

func openLedger() (*iledger.Ledger, error) {
	dir, err := expandHomePath(ledgerDir)
	if err != nil {
		return nil, fmt.Errorf("ledger: expand dir: %w", err)
	}
	l, err := iledger.OpenReadOnly(dir)
	if err != nil {
		// Key not yet generated — fall back to full open which generates the pair.
		return iledger.Open(dir)
	}
	return l, nil
}

func runLedgerList(cmd *cobra.Command, _ []string) error {
	l, err := openLedger()
	if err != nil {
		return err
	}
	ids, err := l.Tasks()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no tasks)")
		return nil
	}
	if ledgerJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(ids)
	}
	for _, id := range ids {
		fmt.Fprintln(cmd.OutOrStdout(), id)
	}
	return nil
}

func runLedgerShow(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	if ledgerTaskID != "" {
		taskID = ledgerTaskID
	}

	dir, err := expandHomePath(ledgerDir)
	if err != nil {
		return fmt.Errorf("ledger show: expand dir: %w", err)
	}
	l, err := iledger.Open(dir)
	if err != nil {
		return err
	}
	entries, err := l.Read(taskID)
	if err != nil {
		return fmt.Errorf("ledger show: %w", err)
	}

	if ledgerJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
	}

	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Task: %s  (%d entries)\n", taskID, len(entries))
	fmt.Fprintln(w, strings.Repeat("-", 80))
	for _, e := range entries {
		ts := e.Ts.Format(time.RFC3339)
		fmt.Fprintf(w, "  %s  %-12s %-24s %s\n", ts, e.Phase, e.Action, e.Result)
	}
	return nil
}

func runLedgerVerify(cmd *cobra.Command, _ []string) error {
	dir, err := expandHomePath(ledgerDir)
	if err != nil {
		return fmt.Errorf("ledger verify: expand dir: %w", err)
	}
	l, err := iledger.Open(dir)
	if err != nil {
		return err
	}
	results, err := l.VerifyAll()
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no tasks to verify")
		return nil
	}

	failed := 0
	w := cmd.OutOrStdout()
	for _, r := range results {
		status := "OK"
		if r.Err != nil {
			status = "FAIL: " + r.Err.Error()
			failed++
		}
		fmt.Fprintf(w, "%-40s  %4d entries  %s\n", r.TaskID, r.Count, status)
	}
	fmt.Fprintf(w, "\n%d tasks verified, %d failed\n", len(results), failed)
	if failed > 0 {
		return fmt.Errorf("ledger: %d task(s) failed signature verification", failed)
	}
	return nil
}

func ledgerSkill() Skill {
	return Skill{
		Name:    "ledger",
		Trigger: "/ledger",
		Summary: "Inspect and verify the signed per-task ledger (list tasks, show entries, verify signatures).",
		Args: []SkillArg{
			{Name: "list", Description: "List all task IDs."},
			{Name: "show <task-id>", Description: "Show entries for a task."},
			{Name: "verify", Description: "Verify ed25519 signatures across all ledger tasks."},
			{Name: "--ledger-dir PATH", Description: "Override ~/.hermes/ledger path."},
			{Name: "--json", Description: "Emit JSON output."},
		},
		Examples: []SkillExample{
			{Description: "List tasks", Command: "fleetctl ledger list"},
			{Description: "Show task entries", Command: "fleetctl ledger show task-abc123"},
			{Description: "Verify all signatures", Command: "fleetctl ledger verify"},
		},
	}
}

func init() { registerSkill("ledger", ledgerSkill) }
