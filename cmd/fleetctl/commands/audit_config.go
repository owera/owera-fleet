// audit_config.go implements `fleetctl audit config` — read the live
// ~/.hermes/config.yaml, compare against the Hermes-recommended baseline
// documented in hermes-setup/SECURITY_NOTES.md, and emit the drift table.
//
// This is the Phase-1 gate item T4.4 contract: the markdown output of
// `fleetctl audit config` against today's live config must reproduce the
// SECURITY_NOTES.md "Drift from documented baseline" table.
package commands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/audit"
)

var (
	auditConfigPath   string
	auditConfigFormat string
)

var auditConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Audit ~/.hermes/config.yaml against the documented Hermes baseline",
	Long: `audit config reads the live ~/.hermes/config.yaml and reports settings
that drift from the Hermes-recommended baseline as documented in
hermes-setup/SECURITY_NOTES.md.

Output formats:
  markdown  pipe-delimited table identical to SECURITY_NOTES.md
  json      []DriftRow for programmatic consumers

Exit code: 0 when no drift; 1 when one or more drift rows fire.`,
	Example: `  fleetctl audit config
  fleetctl audit config --format json
  fleetctl audit config --config /tmp/snapshot.yaml`,
	RunE: runAuditConfig,
}

func init() {
	auditConfigCmd.Flags().StringVar(&auditConfigPath, "config", "~/.hermes/config.yaml", "path to the config.yaml to audit")
	auditConfigCmd.Flags().StringVar(&auditConfigFormat, "format", "markdown", "output format: markdown or json")
	auditCmd.AddCommand(auditConfigCmd)
}

func runAuditConfig(cmd *cobra.Command, _ []string) error {
	resolved, err := expandHomePath(auditConfigPath)
	if err != nil {
		return fmt.Errorf("audit config: expand --config: %w", err)
	}

	rows, err := audit.DetectDrift(cmd.Context(), resolved)
	if err != nil {
		return err
	}

	switch auditConfigFormat {
	case "markdown":
		renderDriftMarkdown(cmd, rows)
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		// Always emit a JSON array, never `null`, so programmatic
		// consumers can len() the result unconditionally.
		if rows == nil {
			rows = []audit.DriftRow{}
		}
		if err := enc.Encode(rows); err != nil {
			return fmt.Errorf("audit config: encode json: %w", err)
		}
	default:
		return fmt.Errorf("audit config: unknown --format %q (want markdown or json)", auditConfigFormat)
	}

	if len(rows) > 0 {
		// Suppress cobra's auto-printed usage on drift — drift is data,
		// not a CLI-usage error. Returning a sentinel non-nil error is
		// the only way cobra propagates a non-zero exit.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return errDriftDetected
	}
	return nil
}

// errDriftDetected is the sentinel returned when at least one drift row
// fires. main.go translates the non-nil RunE error into exit code 1; the
// SilenceErrors flag above keeps cobra from printing the sentinel
// message itself so callers see only the rendered table.
var errDriftDetected = fmt.Errorf("audit config: drift detected")

// renderDriftMarkdown emits the table in the exact column shape that
// hermes-setup/SECURITY_NOTES.md uses, so the output of `fleetctl audit
// config` and the doc are byte-comparable.
func renderDriftMarkdown(cmd *cobra.Command, rows []audit.DriftRow) {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "| Setting | Recommended | Live | What \"live\" actually means in practice |")
	fmt.Fprintln(w, "|---|---|---|---|")
	for _, r := range rows {
		fmt.Fprintf(w, "| %s | %s | %s | %s |\n",
			escapeMDCell(r.Setting),
			escapeMDCell(r.Recommended),
			escapeMDCell(r.Live),
			escapeMDCell(r.Disposition),
		)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "(no drift — every audited setting matches the documented Hermes baseline)")
	}
}

// escapeMDCell escapes characters that would break the markdown table
// layout. Pipe is the only structural one; newlines are normalised to
// spaces so a multi-line disposition still renders as one cell.
func escapeMDCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func auditConfigSkill() Skill {
	return Skill{
		Name:    "audit-config",
		Trigger: "/audit-config",
		Summary: "Audit ~/.hermes/config.yaml against the Hermes-recommended baseline documented in hermes-setup/SECURITY_NOTES.md. Reproduces the SECURITY_NOTES.md drift-from-baseline table from live config and exits 1 when drift is present.",
		Args: []SkillArg{
			{Name: "--config PATH", Description: "Path to the config.yaml to audit (default: ~/.hermes/config.yaml)."},
			{Name: "--format FORMAT", Description: "Output format: markdown (default, matches SECURITY_NOTES.md) or json."},
		},
		Examples: []SkillExample{
			{Description: "Audit the live gateway config", Command: "fleetctl audit config"},
			{Description: "Machine-readable output for CI", Command: "fleetctl audit config --format json"},
			{Description: "Audit a captured snapshot", Command: "fleetctl audit config --config /tmp/snapshot.yaml"},
		},
	}
}

func init() { registerSkill("audit-config", auditConfigSkill) }
