// alert.go implements `fleetctl alert` — fire a test alert through the
// configured backends (PagerDuty / OpsGenie / ntfy.sh). Used to sanity-check
// pager wiring without waiting for a real incident.
package commands

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/alerting"
)

var (
	alertSeverity string
	alertTitle    string
	alertBody     string
	alertSource   string
	alertDedup    string
)

var alertCmd = &cobra.Command{
	Use:   "alert",
	Short: "Fire a test alert through configured backends",
	Long: `alert fires an alert (default severity: critical) through every
configured backend. Backends are read from environment:

  HERMES_PAGERDUTY_ROUTING_KEY  - if set, PagerDuty backend is wired
  HERMES_OPSGENIE_API_KEY       - if set, OpsGenie backend is wired
  HERMES_NTFY_URL               - if set, ntfy.sh backend is wired

Pager backends (PagerDuty, OpsGenie) only fire on --severity=critical.`,
	Example: `  fleetctl alert --title "test alert" --severity critical
  fleetctl alert --title "deploy ok" --severity info`,
	RunE: runAlert,
}

func init() {
	alertCmd.Flags().StringVar(&alertSeverity, "severity", "critical", "alert severity (critical|warning|info)")
	alertCmd.Flags().StringVar(&alertTitle, "title", "", "alert title (required)")
	alertCmd.Flags().StringVar(&alertBody, "body", "", "alert body / description")
	alertCmd.Flags().StringVar(&alertSource, "source", "fleetctl-alert", "alert source identifier")
	alertCmd.Flags().StringVar(&alertDedup, "dedup", "", "dedup key (for backends that support it)")
	rootCmd.AddCommand(alertCmd)
}

// alertRouterFactory is overridable in tests.
var alertRouterFactory = buildAlertRouter

func runAlert(cmd *cobra.Command, _ []string) error {
	if alertTitle == "" {
		return errors.New("alert: --title is required")
	}
	sev := alerting.Severity(alertSeverity)
	switch sev {
	case alerting.SeverityCritical, alerting.SeverityWarning, alerting.SeverityInfo:
	default:
		return fmt.Errorf("alert: invalid --severity %q (want critical|warning|info)", alertSeverity)
	}
	router := alertRouterFactory()
	if len(router.Backends()) == 0 {
		return errors.New("alert: no backends configured (set HERMES_PAGERDUTY_ROUTING_KEY, HERMES_OPSGENIE_API_KEY, or HERMES_NTFY_URL)")
	}

	a := alerting.Alert{
		Severity: sev,
		Title:    alertTitle,
		Body:     alertBody,
		Source:   alertSource,
		Dedup:    alertDedup,
	}
	if err := router.Fire(cmd.Context(), a); err != nil {
		return fmt.Errorf("alert: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Alert fired through: %v\n", router.Backends())
	return nil
}

func buildAlertRouter() *alerting.Router {
	r := alerting.NewRouter()
	if k := os.Getenv("HERMES_PAGERDUTY_ROUTING_KEY"); k != "" {
		r.AddBackend(alerting.NewPagerDuty(k))
	}
	if k := os.Getenv("HERMES_OPSGENIE_API_KEY"); k != "" {
		r.AddBackend(alerting.NewOpsGenie(k))
	}
	if u := os.Getenv("HERMES_NTFY_URL"); u != "" {
		r.AddBackend(alerting.NewNtfy(u))
	}
	return r
}

func alertSkill() Skill {
	return Skill{
		Name:    "alert",
		Trigger: "/alert",
		Summary: "Fire a test alert through configured PagerDuty / OpsGenie / ntfy.sh backends.",
		Args: []SkillArg{
			{Name: "--title TITLE", Description: "Alert title (required)."},
			{Name: "--severity SEV", Description: "critical|warning|info (default critical)."},
			{Name: "--body BODY", Description: "Alert body / description."},
			{Name: "--source SRC", Description: "Alert source identifier."},
			{Name: "--dedup KEY", Description: "Dedup key for supported backends."},
		},
		Examples: []SkillExample{
			{Description: "Critical pager test", Command: "fleetctl alert --title 'pager wiring test' --severity critical"},
			{Description: "Info to chat only", Command: "fleetctl alert --title 'deploy ok' --severity info"},
		},
	}
}

func init() { registerSkill("alert", alertSkill) }
