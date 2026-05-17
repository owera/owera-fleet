// pair.go implements `fleetctl pair` — CRUD over DM pairing tokens that
// identify external callers to the operator plane. Pairings are the identity
// primitive promoted to "tenant" in Phase 3.
package commands

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/budget"
	"github.com/owera/owera-fleet/internal/pairing"
)

var (
	pairDir         string
	pairBudgetDir   string
	pairLabel       string
	pairJSON        bool
	pairMaxReqs     int
	pairMaxCostCents int64
)

var pairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Manage DM pairing tokens (identity primitive for external callers)",
	Long: `pair provides CRUD over the DM pairing tokens stored under
~/.hermes/pairings/. Each pairing has an opaque ID, a secret bearer token,
and an optional human-readable label. Tokens are returned only at create
time and can be revoked but not regenerated.`,
}

var pairCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new pairing",
	RunE:  runPairCreate,
}

var pairListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all pairings",
	RunE:  runPairList,
}

var pairShowCmd = &cobra.Command{
	Use:   "show <pairing-id>",
	Short: "Show a pairing including its budget state",
	Args:  cobra.ExactArgs(1),
	RunE:  runPairShow,
}

var pairRevokeCmd = &cobra.Command{
	Use:   "revoke <pairing-id>",
	Short: "Revoke a pairing (idempotent)",
	Args:  cobra.ExactArgs(1),
	RunE:  runPairRevoke,
}

var pairBudgetCmd = &cobra.Command{
	Use:   "budget <pairing-id>",
	Short: "Update budget limits for a pairing",
	Args:  cobra.ExactArgs(1),
	RunE:  runPairBudget,
}

func init() {
	pairCmd.PersistentFlags().StringVar(&pairDir, "pairing-dir", "~/.hermes/pairings", "pairing storage directory")
	pairCmd.PersistentFlags().StringVar(&pairBudgetDir, "budget-dir", "~/.hermes/budgets", "budget storage directory")
	pairCmd.PersistentFlags().BoolVar(&pairJSON, "json", false, "emit JSON output")

	pairCreateCmd.Flags().StringVar(&pairLabel, "label", "", "human-readable label")

	pairBudgetCmd.Flags().IntVar(&pairMaxReqs, "max-requests", 0, "daily request limit (0 = keep current)")
	pairBudgetCmd.Flags().Int64Var(&pairMaxCostCents, "max-cost-cents", 0, "daily cost cap in USD cents (0 = keep current)")

	pairCmd.AddCommand(pairCreateCmd, pairListCmd, pairShowCmd, pairRevokeCmd, pairBudgetCmd)
	rootCmd.AddCommand(pairCmd)
}

func openPairingStore() (*pairing.Store, error) {
	dir, err := expandHomePath(pairDir)
	if err != nil {
		return nil, fmt.Errorf("pair: expand dir: %w", err)
	}
	return pairing.NewStore(dir)
}

func openBudgetStore() (*budget.Store, error) {
	dir, err := expandHomePath(pairBudgetDir)
	if err != nil {
		return nil, fmt.Errorf("pair: expand budget dir: %w", err)
	}
	return budget.NewStore(dir)
}

func runPairCreate(cmd *cobra.Command, _ []string) error {
	s, err := openPairingStore()
	if err != nil {
		return err
	}
	p, err := s.Create(pairLabel)
	if err != nil {
		return err
	}
	if pairJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Pairing created.\n  ID:    %s\n  Label: %s\n  Token: %s\n", p.ID, p.Label, p.Token)
	fmt.Fprintln(cmd.OutOrStdout(), "\nStore this token securely — it will not be shown again.")
	return nil
}

func runPairList(cmd *cobra.Command, _ []string) error {
	s, err := openPairingStore()
	if err != nil {
		return err
	}
	all, err := s.List()
	if err != nil {
		return err
	}
	if pairJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(all)
	}
	if len(all) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no pairings)")
		return nil
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%-12s %-20s %-10s %s\n", "ID", "Label", "Active", "Created")
	fmt.Fprintln(w, strings.Repeat("-", 70))
	for _, p := range all {
		label := p.Label
		if label == "" {
			label = "(unlabeled)"
		}
		active := "yes"
		if !p.Active {
			active = "REVOKED"
		}
		fmt.Fprintf(w, "%-12s %-20s %-10s %s\n", p.ID, truncateAction(label), active, p.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func runPairShow(cmd *cobra.Command, args []string) error {
	id := args[0]
	s, err := openPairingStore()
	if err != nil {
		return err
	}
	p, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("pair show: %w", err)
	}
	bs, err := openBudgetStore()
	if err != nil {
		return err
	}
	st, err := bs.Get(id)
	if err != nil {
		return err
	}
	if pairJSON {
		out := map[string]any{"pairing": p, "budget": st}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Pairing %s\n", p.ID)
	fmt.Fprintf(w, "  Label:      %s\n", p.Label)
	fmt.Fprintf(w, "  Active:     %v\n", p.Active)
	fmt.Fprintf(w, "  Created:    %s\n", p.CreatedAt.Format(time.RFC3339))
	if !p.RevokedAt.IsZero() {
		fmt.Fprintf(w, "  Revoked:    %s\n", p.RevokedAt.Format(time.RFC3339))
	}
	fmt.Fprintln(w, "\nBudget (today):")
	fmt.Fprintf(w, "  Requests:   %d/%d\n", st.Requests, st.MaxRequests)
	fmt.Fprintf(w, "  Cost:       $%d.%02d/$%d.%02d\n",
		st.CostCents/100, st.CostCents%100,
		st.MaxCostCents/100, st.MaxCostCents%100,
	)
	return nil
}

func runPairRevoke(cmd *cobra.Command, args []string) error {
	id := args[0]
	s, err := openPairingStore()
	if err != nil {
		return err
	}
	if err := s.Revoke(id); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Pairing %s revoked.\n", id)
	return nil
}

func runPairBudget(cmd *cobra.Command, args []string) error {
	id := args[0]
	bs, err := openBudgetStore()
	if err != nil {
		return err
	}
	if err := bs.SetLimits(id, pairMaxReqs, pairMaxCostCents); err != nil {
		return err
	}
	st, _ := bs.Get(id)
	fmt.Fprintf(cmd.OutOrStdout(), "Budget for %s updated: %d req/day, $%d.%02d/day cap\n",
		id, st.MaxRequests, st.MaxCostCents/100, st.MaxCostCents%100,
	)
	return nil
}

func pairSkill() Skill {
	return Skill{
		Name:    "pair",
		Trigger: "/pair",
		Summary: "Manage DM pairing tokens (CRUD + budget limits for external callers).",
		Args: []SkillArg{
			{Name: "create [--label NAME]", Description: "Create a new pairing; token is shown once."},
			{Name: "list", Description: "List all pairings."},
			{Name: "show <id>", Description: "Show pairing details + today's budget state."},
			{Name: "revoke <id>", Description: "Revoke a pairing (idempotent)."},
			{Name: "budget <id> --max-requests N --max-cost-cents N", Description: "Update daily limits."},
		},
		Examples: []SkillExample{
			{Description: "Create labeled pairing", Command: "fleetctl pair create --label \"acme-corp-prod\""},
			{Description: "List pairings", Command: "fleetctl pair list"},
			{Description: "Set $50/day cap", Command: "fleetctl pair budget abc123 --max-cost-cents 5000"},
		},
	}
}

func init() { registerSkill("pair", pairSkill) }
