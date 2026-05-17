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
	ledgerDir         string
	ledgerTaskID      string
	ledgerJSON        bool
	ledgerShowTenant  bool
	ledgerByTenantID  string
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
	Short: "Show signed entries for a task (or for a tenant via --by-tenant)",
	// 0..1 args: 0 is only valid when --by-tenant is supplied (the cross-task
	// tenant view). Validation of the combination happens in runLedgerShow
	// so the user gets a precise error rather than cobra's generic count check.
	Args: cobra.MaximumNArgs(1),
	RunE: runLedgerShow,
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
	// --show-tenant adds a tenant_id column to the column-display variant of
	// `ledger show`. The JSON variant always emits the field (via
	// omitempty) so machine consumers — notably the customer plane's
	// Stripe reconciliation — don't need a flag to surface it.
	ledgerShowCmd.Flags().BoolVar(&ledgerShowTenant, "show-tenant", false, "include the tenant_id column in human-readable show output")
	// --by-tenant filters `ledger show` to entries whose tenant_id equals
	// the value, scanning every task file. The positional task-id arg is
	// ignored when this flag is set. Pass --by-tenant "" to surface
	// operator-only entries (TenantID unset).
	ledgerShowCmd.Flags().StringVar(&ledgerByTenantID, "by-tenant", "", "show entries whose tenant_id matches (across all task files)")
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
	// --by-tenant is its own selection mode (cross-task) and is mutually
	// exclusive with a positional task-id / --task arg. Was-set is keyed off
	// cobra's Changed bit rather than the empty string, because empty is a
	// meaningful tenant value (operator-only entries).
	byTenantSet := cmd.Flags().Changed("by-tenant")

	var taskID string
	if len(args) == 1 {
		taskID = args[0]
	}
	if ledgerTaskID != "" {
		taskID = ledgerTaskID
	}

	if byTenantSet && taskID != "" {
		return fmt.Errorf("ledger show: --by-tenant is mutually exclusive with a task-id arg")
	}
	if !byTenantSet && taskID == "" {
		return fmt.Errorf("ledger show: a task-id arg (or --by-tenant) is required")
	}

	dir, err := expandHomePath(ledgerDir)
	if err != nil {
		return fmt.Errorf("ledger show: expand dir: %w", err)
	}
	l, err := iledger.Open(dir)
	if err != nil {
		return err
	}

	var entries []iledger.Entry
	if byTenantSet {
		entries, err = l.ReadByTenant(ledgerByTenantID)
	} else {
		entries, err = l.Read(taskID)
	}
	if err != nil {
		return fmt.Errorf("ledger show: %w", err)
	}

	if ledgerJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
	}

	w := cmd.OutOrStdout()
	if byTenantSet {
		fmt.Fprintf(w, "Tenant: %q  (%d entries across %d task(s))\n", ledgerByTenantID, len(entries), countTaskIDs(entries))
	} else {
		fmt.Fprintf(w, "Task: %s  (%d entries)\n", taskID, len(entries))
	}
	fmt.Fprintln(w, strings.Repeat("-", 80))
	// Column layout: when --show-tenant is set, a 24-char tenant column is
	// injected after `result`; otherwise the layout is bit-identical to the
	// pre-issue-#5 form so existing operator muscle memory still works.
	for _, e := range entries {
		ts := e.Ts.Format(time.RFC3339)
		if ledgerShowTenant {
			tenant := e.TenantID
			if tenant == "" {
				tenant = "-"
			}
			fmt.Fprintf(w, "  %s  %-12s %-24s %-8s %s\n", ts, e.Phase, e.Action, e.Result, tenant)
		} else {
			fmt.Fprintf(w, "  %s  %-12s %-24s %s\n", ts, e.Phase, e.Action, e.Result)
		}
	}
	return nil
}

// countTaskIDs returns the number of distinct TaskIDs in entries. Used only
// for the --by-tenant header line.
func countTaskIDs(entries []iledger.Entry) int {
	seen := map[string]struct{}{}
	for _, e := range entries {
		seen[e.TaskID] = struct{}{}
	}
	return len(seen)
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
			{Name: "--json", Description: "Emit JSON output (always includes tenant_id when set)."},
			{Name: "--show-tenant", Description: "Include the tenant_id column in `ledger show` table output."},
			{Name: "--by-tenant ID", Description: "On `ledger show`: filter to entries whose tenant_id matches (cross-task)."},
		},
		Examples: []SkillExample{
			{Description: "List tasks", Command: "fleetctl ledger list"},
			{Description: "Show task entries", Command: "fleetctl ledger show task-abc123"},
			{Description: "Show with tenant column", Command: "fleetctl ledger show task-abc123 --show-tenant"},
			{Description: "Cross-task tenant view", Command: "fleetctl ledger show --by-tenant tenant-acme"},
			{Description: "Verify all signatures", Command: "fleetctl ledger verify"},
		},
	}
}

func init() { registerSkill("ledger", ledgerSkill) }
