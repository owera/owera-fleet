// metrics_cmd.go implements `fleetctl metrics` — Prometheus text exposition
// of fleet metrics derived from JSONL logs, ledger entries, and heartbeats.
package commands

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/metrics"
)

var (
	metricsAddr    string
	metricsLogDir  string
	metricsServe   bool
	metricsScrape  bool
)

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Serve or scrape Prometheus-format fleet metrics",
	Long: `metrics derives Prometheus metrics from JSONL logs, ledger entries,
and heartbeat state. The metric surface is small and stable:

  fleet_heartbeat_age_seconds{node}
  fleet_actions_total{phase,result}
  fleet_action_error_rate{phase}
  fleet_action_duration_ms_p50{phase}
  fleet_action_duration_ms_p95{phase}

Use --serve to expose an HTTP /metrics endpoint, or --scrape to print the
current text exposition once and exit.`,
	RunE: runMetrics,
}

func init() {
	metricsCmd.Flags().StringVar(&metricsAddr, "addr", "127.0.0.1:9090", "listen address for --serve")
	metricsCmd.Flags().StringVar(&metricsLogDir, "log-dir", "~/.hermes/logs", "JSONL log directory to scan")
	metricsCmd.Flags().BoolVar(&metricsServe, "serve", false, "start an HTTP /metrics server")
	metricsCmd.Flags().BoolVar(&metricsScrape, "scrape", false, "print current metrics once and exit")
	rootCmd.AddCommand(metricsCmd)
}

func runMetrics(cmd *cobra.Command, _ []string) error {
	if !metricsServe && !metricsScrape {
		return errors.New("metrics: --serve or --scrape is required")
	}
	logDir, err := expandHomePath(metricsLogDir)
	if err != nil {
		return err
	}
	collector := metrics.New(logDir)

	if metricsScrape {
		ms, err := collector.Collect()
		if err != nil {
			return err
		}
		return metrics.Render(cmd.OutOrStdout(), ms)
	}

	srv, err := metrics.Serve(metricsAddr, collector)
	if err != nil {
		return err
	}
	defer srv.Close()
	fmt.Fprintf(cmd.OutOrStdout(), "metrics: serving on http://%s/metrics (Ctrl-C to stop)\n", metricsAddr)

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	<-sigC
	fmt.Fprintln(cmd.OutOrStdout(), "metrics: stopping")
	return nil
}

func metricsSkill() Skill {
	return Skill{
		Name:    "metrics",
		Trigger: "/metrics",
		Summary: "Serve or scrape Prometheus-format metrics derived from JSONL logs, ledger, heartbeats.",
		Args: []SkillArg{
			{Name: "--serve", Description: "Start an HTTP /metrics server."},
			{Name: "--scrape", Description: "Print current metrics once and exit."},
			{Name: "--addr ADDR", Description: "Listen address (default 127.0.0.1:9090)."},
			{Name: "--log-dir PATH", Description: "Override ~/.hermes/logs path."},
		},
		Examples: []SkillExample{
			{Description: "Start metrics server", Command: "fleetctl metrics --serve"},
			{Description: "Scrape once", Command: "fleetctl metrics --scrape"},
		},
	}
}

func init() { registerSkill("metrics", metricsSkill) }
