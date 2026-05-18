// watchdog.go implements `fleetctl watchdog` — the gateway-side
// staleness monitor that consumes ~/.hermes/heartbeats/<host>.json
// mtimes (produced by heartbeats-bridge) and fires alerts through
// internal/alerting when a worker stops heart-beating.
//
// # Background
//
// hermes-setup era: `scripts/heartbeat-watchdog.sh` (com.hermes.watchdog)
// SSH-probes each worker every 120s + fires ntfy.sh alerts on stale.
// That script has a long-standing ssh-drains-stdin bug that silently
// skipped every node after the first; see hermes-setup commit log.
//
// owera-fleet era: this command + com.owera.watchdog supersede the
// bash. The split is intentional:
//
//   - heartbeats-bridge does the SSH probe + writes per-host gateway
//     files (~/.hermes/heartbeats/<host>.json) every --interval.
//   - watchdog reads those local files. No SSH, no nodes.txt parsing.
//     The local mtime is the single source of truth for "when did we
//     last see this worker alive?" — matching what fleet.HealthSnapshot
//     and Prometheus consume via internal/metrics.HeartbeatAges.
//
// This split lets one component stay narrow (probe + record) while the
// other (decide + alert) can be tested without SSH or worker mocks.
//
// # Alerting wiring
//
// The watchdog fires via internal/alerting.Router. Backends are
// configured from environment at startup, mirroring `fleetctl alert`:
//
//	HERMES_PAGERDUTY_ROUTING_KEY  - PagerDuty Events API v2
//	HERMES_OPSGENIE_API_KEY       - OpsGenie Alerts API
//	HERMES_NTFY_URL               - ntfy.sh topic URL
//
// If none are set, the watchdog runs (and logs stale/recovered
// transitions) but emits no external alerts — useful for local
// development and CI smoke.

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/alerting"
	"github.com/owera/owera-fleet/internal/launchd"
	"github.com/owera/owera-fleet/internal/log"
)

var (
	watchdogInterval      time.Duration
	watchdogStaleMax      time.Duration
	watchdogDedupWindow   time.Duration
	watchdogOnce          bool
	watchdogHeartbeatsDir = "~/.hermes/heartbeats"
	watchdogStateDir      = "~/.hermes/watchdog"
	watchdogLogPath       = "~/.hermes/logs/owera-watchdog.jsonl"
)

const (
	oweraWatchdogLabel        = "com.owera.watchdog"
	oweraWatchdogTemplateName = launchd.TemplateOweraWatchdog
)

var watchdogCmd = &cobra.Command{
	Use:   "watchdog",
	Short: "Fire alerts when ~/.hermes/heartbeats/<host>.json mtimes go stale",
	Long: `watchdog scans every file under ~/.hermes/heartbeats/*.json on
each tick and fires a Critical alert (via internal/alerting) when a
worker's mtime is older than --stale-max. A previously-stale worker
returning fresh fires a Warning "recovered" alert. Per-host dedup
suppresses repeat Critical alerts within --dedup-window.

Defaults match the legacy bash watchdog:
  --interval       120s
  --stale-max       5m
  --dedup-window    1h

The process runs until SIGINT/SIGTERM. Pass --once to run a single
sweep and exit non-zero if any host is stale.

Alert backends are read from environment at startup:
  HERMES_PAGERDUTY_ROUTING_KEY    PagerDuty Events API v2
  HERMES_OPSGENIE_API_KEY         OpsGenie Alerts API
  HERMES_NTFY_URL                 ntfy.sh topic URL (e.g. https://ntfy.sh/owera-…)

If none are set, watchdog still logs stale/recovered transitions but
emits no external alerts (useful for local dev / CI smoke).`,
	Example: `  fleetctl watchdog
  fleetctl watchdog --interval 60s --stale-max 3m
  fleetctl watchdog --once  # smoke check`,
	RunE: runWatchdog,
}

func init() {
	watchdogCmd.Flags().DurationVar(&watchdogInterval, "interval", 120*time.Second, "scan interval (≥10s)")
	watchdogCmd.Flags().DurationVar(&watchdogStaleMax, "stale-max", 5*time.Minute, "treat heartbeat files older than this as stale")
	watchdogCmd.Flags().DurationVar(&watchdogDedupWindow, "dedup-window", 1*time.Hour, "minimum gap between repeat stale alerts for the same host")
	watchdogCmd.Flags().BoolVar(&watchdogOnce, "once", false, "run a single scan and exit (exit non-zero if any host is stale)")
	rootCmd.AddCommand(watchdogCmd)
	registerSkill("watchdog", watchdogSkill)
}

func runWatchdog(cmd *cobra.Command, _ []string) error {
	if watchdogInterval < 10*time.Second {
		return fmt.Errorf("watchdog: interval %s < 10s", watchdogInterval)
	}
	heartbeatsDir, err := expandHomePath(watchdogHeartbeatsDir)
	if err != nil {
		return err
	}
	stateDir, err := expandHomePath(watchdogStateDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("watchdog: mkdir %s: %w", stateDir, err)
	}
	logPath, err := expandHomePath(watchdogLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(logPath)
	if err != nil {
		return fmt.Errorf("watchdog: open log: %w", err)
	}
	defer logger.Close()

	router, backendNames := buildAlertingRouterFromEnv()
	fmt.Fprintf(cmd.OutOrStdout(), "fleetctl watchdog: backends=%s stale-max=%s dedup-window=%s\n",
		joinOrNone(backendNames), watchdogStaleMax, watchdogDedupWindow)

	if watchdogOnce {
		return scanOnce(cmd.Context(), heartbeatsDir, stateDir, watchdogStaleMax, watchdogDedupWindow, router, logger)
	}
	return scanLoop(cmd, heartbeatsDir, stateDir, watchdogStaleMax, watchdogDedupWindow, watchdogInterval, router, logger)
}

// buildAlertingRouterFromEnv wires PagerDuty, OpsGenie, and ntfy.sh
// backends from environment variables. The returned slice of backend
// names is for the boot-log line so operators can verify the wiring
// at startup without grepping env.
func buildAlertingRouterFromEnv() (*alerting.Router, []string) {
	r := alerting.NewRouter()
	var names []string
	if key := os.Getenv("HERMES_PAGERDUTY_ROUTING_KEY"); key != "" {
		r.AddBackend(alerting.NewPagerDuty(key))
		names = append(names, "pagerduty")
	}
	if key := os.Getenv("HERMES_OPSGENIE_API_KEY"); key != "" {
		r.AddBackend(alerting.NewOpsGenie(key))
		names = append(names, "opsgenie")
	}
	if url := os.Getenv("HERMES_NTFY_URL"); url != "" {
		r.AddBackend(alerting.NewNtfy(url))
		names = append(names, "ntfy")
	}
	return r, names
}

func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "(none — log-only mode)"
	}
	return strings.Join(names, "+")
}

// scanOnce runs one full scan. Returns an error if ANY host is stale —
// useful for --once smoke checks in CI.
func scanOnce(ctx context.Context, heartbeatsDir, stateDir string, staleMax, dedupWindow time.Duration, router *alerting.Router, logger *log.Logger) error {
	stale, _ := scanCycle(ctx, heartbeatsDir, stateDir, staleMax, dedupWindow, router, logger)
	if stale > 0 {
		return fmt.Errorf("watchdog: %d host(s) stale", stale)
	}
	return nil
}

// scanLoop runs scans on a ticker until SIGINT/SIGTERM. Failures
// (filesystem errors, alert-send errors) are logged but never halt
// the loop.
func scanLoop(cmd *cobra.Command, heartbeatsDir, stateDir string, staleMax, dedupWindow, interval time.Duration, router *alerting.Router, logger *log.Logger) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var consecutiveErrors atomic.Int64
	doScan := func() {
		ctx, cancel := context.WithTimeout(cmd.Context(), interval-time.Second)
		defer cancel()
		_, anyErr := scanCycle(ctx, heartbeatsDir, stateDir, staleMax, dedupWindow, router, logger)
		if anyErr {
			consecutiveErrors.Add(1)
		} else {
			consecutiveErrors.Store(0)
		}
	}

	doScan() // immediate first scan
	fmt.Fprintf(cmd.OutOrStdout(), "fleetctl watchdog: scanning %s every %s\n",
		heartbeatsDir, interval)
	for {
		select {
		case <-sigCh:
			fmt.Fprintln(cmd.OutOrStdout(), "fleetctl watchdog: shutting down")
			return nil
		case <-ticker.C:
			doScan()
		}
	}
}

// scanCycle is one scan over heartbeatsDir. Returns (staleCount, anyErr).
// staleCount is the number of hosts whose files were older than staleMax
// at this tick (including ones suppressed by dedup). anyErr is true if
// any filesystem or alert-send error occurred.
func scanCycle(ctx context.Context, heartbeatsDir, stateDir string, staleMax, dedupWindow time.Duration, router *alerting.Router, logger *log.Logger) (int, bool) {
	now := time.Now().UTC()
	entries, err := readHeartbeatDir(heartbeatsDir)
	if err != nil {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "watchdog",
			Action: "read-dir", Result: log.ResultError, StderrTail: err.Error(),
		})
		return 0, true
	}
	if len(entries) == 0 {
		_ = logger.Action(log.Event{
			Node: "gateway", Phase: "watchdog",
			Action: "scan", Result: log.ResultWarn,
			StderrTail: fmt.Sprintf("no heartbeat files in %s", heartbeatsDir),
		})
		return 0, false
	}

	staleCount := 0
	anyErr := false
	for _, e := range entries {
		age := now.Sub(e.ModTime)
		isStale := age > staleMax
		markerPath := filepath.Join(stateDir, ".alerted-"+e.Host)
		markerTime, hasMarker := readMarkerTime(markerPath)

		switch {
		case isStale:
			staleCount++
			if hasMarker && now.Sub(markerTime) < dedupWindow {
				_ = logger.Action(log.Event{
					Node: e.Host, Phase: "watchdog",
					Action: "stale-suppressed", Result: log.ResultOK,
					StderrTail: fmt.Sprintf("age=%s gap=%s window=%s", age.Round(time.Second), now.Sub(markerTime).Round(time.Second), dedupWindow),
				})
				continue
			}
			alert := alerting.Alert{
				Severity:   alerting.SeverityCritical,
				Source:     "watchdog",
				Dedup:      e.Host,
				Title:      fmt.Sprintf("Hermes worker %s stale", e.Host),
				Body:       fmt.Sprintf("Heartbeat file mtime %s is %s old (stale-max %s)", e.ModTime.UTC().Format(time.RFC3339), age.Round(time.Second), staleMax),
				Labels:     map[string]string{"host": e.Host, "age_seconds": strconv.FormatInt(int64(age.Seconds()), 10)},
				OccurredAt: now,
			}
			if err := router.Fire(ctx, alert); err != nil {
				anyErr = true
				_ = logger.Action(log.Event{
					Node: e.Host, Phase: "watchdog",
					Action: "alert-stale", Result: log.ResultError, StderrTail: err.Error(),
				})
				// Don't write the marker on failure — try again next cycle.
				continue
			}
			if err := writeMarker(markerPath, now); err != nil {
				anyErr = true
				_ = logger.Action(log.Event{
					Node: e.Host, Phase: "watchdog",
					Action: "write-marker", Result: log.ResultError, StderrTail: err.Error(),
				})
				continue
			}
			_ = logger.Action(log.Event{
				Node: e.Host, Phase: "watchdog",
				Action: "alert-stale", Result: log.ResultOK,
				StderrTail: fmt.Sprintf("age=%s", age.Round(time.Second)),
			})

		case hasMarker:
			// Fresh + marker present → host just recovered.
			alert := alerting.Alert{
				Severity:   alerting.SeverityWarning,
				Source:     "watchdog",
				Dedup:      e.Host,
				Title:      fmt.Sprintf("Hermes worker %s recovered", e.Host),
				Body:       fmt.Sprintf("Heartbeat file mtime %s is %s old (within stale-max %s)", e.ModTime.UTC().Format(time.RFC3339), age.Round(time.Second), staleMax),
				Labels:     map[string]string{"host": e.Host},
				OccurredAt: now,
			}
			if err := router.Fire(ctx, alert); err != nil {
				anyErr = true
				_ = logger.Action(log.Event{
					Node: e.Host, Phase: "watchdog",
					Action: "alert-recovered", Result: log.ResultError, StderrTail: err.Error(),
				})
				continue
			}
			if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				anyErr = true
				_ = logger.Action(log.Event{
					Node: e.Host, Phase: "watchdog",
					Action: "remove-marker", Result: log.ResultError, StderrTail: err.Error(),
				})
				continue
			}
			_ = logger.Action(log.Event{
				Node: e.Host, Phase: "watchdog",
				Action: "alert-recovered", Result: log.ResultOK,
				StderrTail: fmt.Sprintf("age=%s", age.Round(time.Second)),
			})

		default:
			// Fresh + no marker: no-op. Don't log on the happy path —
			// healthy fleets shouldn't produce a log line per host per
			// tick.
		}
	}
	return staleCount, anyErr
}

// hbEntry is one heartbeat file's parsed metadata.
type hbEntry struct {
	Host    string
	ModTime time.Time
}

// readHeartbeatDir lists heartbeat files in dir, deriving Host from the
// filename ("<host>.json" -> "<host>"). Skips entries that don't end
// in .json or are hidden (start with ".").
func readHeartbeatDir(dir string) ([]hbEntry, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]hbEntry, 0, len(ents))
	for _, ent := range ents {
		name := ent.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		host := strings.TrimSuffix(name, ".json")
		out = append(out, hbEntry{Host: host, ModTime: info.ModTime()})
	}
	// Sort by host for deterministic ordering in tests + log output.
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// readMarkerTime returns the mtime stored in the marker file at path,
// plus true if the file exists. Errors map to (zero, false) so the
// caller treats missing/unreadable markers the same way.
func readMarkerTime(path string) (time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// writeMarker creates (or truncates) the marker file at path with the
// given mtime as both the body (RFC3339 for human-readability) and the
// file mtime (used as the dedup state itself).
func writeMarker(path string, when time.Time) error {
	if err := os.WriteFile(path, []byte(when.UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, when, when)
}

// --- install / uninstall / status (mirror heartbeats-bridge pattern) ---

func oweraWatchdogPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", oweraWatchdogLabel+".plist"), nil
}

// newWatchdogManagerFn lets tests substitute a Manager with mock
// RunCmd / WriteFile.
var newWatchdogManagerFn = func() *launchd.Manager { return launchd.New(nil) }

var watchdogInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install com.owera.watchdog as a user LaunchAgent",
	RunE:  runWatchdogInstall,
}

func runWatchdogInstall(cmd *cobra.Command, _ []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("install: home dir: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("install: os.Executable: %w", err)
	}
	exeAbs, err := filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("install: abs: %w", err)
	}
	plistPath, err := oweraWatchdogPlistPath()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	m := newWatchdogManagerFn()
	body, err := m.RenderTemplate(oweraWatchdogTemplateName, launchd.Vars{
		Label:      oweraWatchdogLabel,
		HomeDir:    home,
		ScriptPath: exeAbs,
	})
	if err != nil {
		return fmt.Errorf("install: render: %w", err)
	}
	if err := m.Install(cmd.Context(), body, plistPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"watchdog: installed at %s (label=%s, exe=%s)\n"+
			"watchdog: configure alert backends via launchctl setenv before reload, e.g.:\n"+
			"  launchctl setenv HERMES_NTFY_URL https://ntfy.sh/owera-…\n"+
			"  launchctl setenv HERMES_PAGERDUTY_ROUTING_KEY <key>\n"+
			"  launchctl kickstart -k gui/$(id -u)/%s\n",
		plistPath, oweraWatchdogLabel, exeAbs, oweraWatchdogLabel)
	return nil
}

var watchdogUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Tear down the com.owera.watchdog LaunchAgent",
	RunE:  runWatchdogUninstall,
}

func runWatchdogUninstall(cmd *cobra.Command, _ []string) error {
	plistPath, err := oweraWatchdogPlistPath()
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	m := newWatchdogManagerFn()
	if err := m.Uninstall(cmd.Context(), oweraWatchdogLabel, plistPath); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"watchdog: uninstalled (%s, label=%s)\n",
		plistPath, oweraWatchdogLabel)
	return nil
}

var watchdogStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print the running state of com.owera.watchdog",
	RunE:  runWatchdogStatus,
}

func runWatchdogStatus(cmd *cobra.Command, _ []string) error {
	m := newWatchdogManagerFn()
	st, err := m.Status(cmd.Context(), oweraWatchdogLabel)
	if err != nil {
		if errors.Is(err, launchd.ErrUnloaded) {
			fmt.Fprintf(cmd.OutOrStdout(), "watchdog: not loaded (label=%s)\n", oweraWatchdogLabel)
			return fmt.Errorf("not loaded")
		}
		return fmt.Errorf("status: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"watchdog: label=%s loaded=%t pid=%s last_exit=%s state=%s\n",
		st.Label, st.Loaded, st.PID, st.LastExitCode, st.State)
	return nil
}

func init() {
	watchdogCmd.AddCommand(watchdogInstallCmd)
	watchdogCmd.AddCommand(watchdogUninstallCmd)
	watchdogCmd.AddCommand(watchdogStatusCmd)
}

func watchdogSkill() Skill {
	return Skill{
		Name:    "watchdog",
		Trigger: "/watchdog",
		Summary: "Fire alerts when ~/.hermes/heartbeats/<host>.json mtimes go stale (replaces the hermes-setup bash watchdog).",
		Args: []SkillArg{
			{Name: "--interval DUR", Description: "Scan interval (≥10s). Default 120s."},
			{Name: "--stale-max DUR", Description: "Treat heartbeats older than this as stale. Default 5m."},
			{Name: "--dedup-window DUR", Description: "Minimum gap between repeat stale alerts per host. Default 1h."},
			{Name: "--once", Description: "Run a single scan and exit non-zero if any host is stale."},
			{Name: "install / uninstall / status", Description: "launchd lifecycle for com.owera.watchdog."},
		},
	}
}
