// setup_agent.go implements `fleetctl setup-agent` — install, uninstall, or
// query the status of a single com.hermes.* LaunchAgent on the gateway.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/launchd"
	"github.com/owera/owera-fleet/internal/log"
)

var setupAgentLogPath = "~/.hermes/logs/setup-agent.jsonl"

var (
	saInstall       bool
	saUninstall     bool
	saHermesDir     string
	saRepoDir       string
	saAlertURL      string
	saResticRepo    string
	saResticPassCmd string
	saUser          string
	saNodeLabel     string
	saJSON          bool
	saQuiet         bool
)

// saManager is the testable subset of *launchd.Manager.
type saManager interface {
	Install(ctx context.Context, plist []byte, path string) error
	Uninstall(ctx context.Context, label, path string) error
	Status(ctx context.Context, label string) (launchd.Status, error)
	RenderTemplate(name string, vars launchd.Vars) ([]byte, error)
}

// newSAManager is overridable in tests.
var newSAManager = func(logger *log.Logger) saManager {
	return launchd.New(logger)
}

type svcSpec struct {
	label    string
	tmpl     string
	scriptFn string // relative path under RepoDir/scripts/
}

var knownSvcs = map[string]svcSpec{
	"heartbeat":     {"com.hermes.heartbeat", launchd.TemplateHeartbeat, ""},
	"backup":        {"com.hermes.backup", launchd.TemplateBackup, "backup-hermes-state.sh"},
	"backup-worker": {"com.hermes.backup-worker", launchd.TemplateBackupWorker, "backup-worker-state.sh"},
	"watchdog":      {"com.hermes.watchdog", launchd.TemplateWatchdog, "heartbeat-watchdog.sh"},
	"logrotate":     {"com.hermes.logrotate", launchd.TemplateLogrotate, "rotate-hermes-logs.sh"},
}

var setupAgentCobraCmd = &cobra.Command{
	Use:   "setup-agent <service>",
	Short: "Install, uninstall, or report status of a com.hermes.* LaunchAgent",
	Long: `setup-agent manages the macOS LaunchAgent lifecycle for one Hermes service.
<service> must be one of: heartbeat, backup, backup-worker, watchdog, logrotate.

Default action is status (read-only). Supply --install or --uninstall to
mutate. Every mutation is appended to ~/.hermes/logs/setup-agent.jsonl in the
canonical Owera JSONL schema.`,
	Example: `  fleetctl setup-agent heartbeat
  fleetctl setup-agent backup --install --repo-dir ~/hermes-setup
  fleetctl setup-agent watchdog --uninstall`,
	Args: cobra.ExactArgs(1),
	RunE: runSetupAgent,
}

func init() {
	setupAgentCobraCmd.Flags().BoolVar(&saInstall, "install", false, "install + load the LaunchAgent")
	setupAgentCobraCmd.Flags().BoolVar(&saUninstall, "uninstall", false, "unload + remove the LaunchAgent")
	setupAgentCobraCmd.Flags().StringVar(&saHermesDir, "hermes-dir", "", "override ~/.hermes for log destination")
	setupAgentCobraCmd.Flags().StringVar(&saRepoDir, "repo-dir", "", "hermes-setup checkout root (required for --install of script agents)")
	setupAgentCobraCmd.Flags().StringVar(&saAlertURL, "alert-url", "", "ntfy alert URL for watchdog --install")
	setupAgentCobraCmd.Flags().StringVar(&saResticRepo, "restic-repository", "", "restic repo URI for backup --install")
	setupAgentCobraCmd.Flags().StringVar(&saResticPassCmd, "restic-password-command", "", "restic password command for backup --install")
	setupAgentCobraCmd.Flags().StringVar(&saUser, "user", "hermes", "worker username for heartbeat plist")
	setupAgentCobraCmd.Flags().StringVar(&saNodeLabel, "node-label", "", "heartbeat node label (default: hostname short form)")
	setupAgentCobraCmd.Flags().BoolVar(&saJSON, "json", false, "emit JSON result to stdout")
	setupAgentCobraCmd.Flags().BoolVar(&saQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	rootCmd.AddCommand(setupAgentCobraCmd)
}

func runSetupAgent(cmd *cobra.Command, args []string) error {
	svcName := args[0]
	spec, ok := knownSvcs[svcName]
	if !ok {
		return fmt.Errorf("setup-agent: unknown service %q; must be one of: %s",
			svcName, strings.Join(sortedSvcNames(), ", "))
	}
	if saInstall && saUninstall {
		return errors.New("setup-agent: --install and --uninstall are mutually exclusive")
	}

	// Resolve log path.
	logSrc := setupAgentLogPath
	if saHermesDir != "" {
		logSrc = filepath.Join(saHermesDir, "logs", "setup-agent.jsonl")
	}
	resolvedLog, err := expandHomePath(logSrc)
	if err != nil {
		return fmt.Errorf("setup-agent: expand log path: %w", err)
	}

	// Open logger only for mutating actions.
	var logger *log.Logger
	if saInstall || saUninstall {
		logger, err = openLogger(resolvedLog)
		if err != nil {
			return fmt.Errorf("setup-agent: open log: %w", err)
		}
		defer logger.Close()
	}

	mgr := newSAManager(logger)
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	switch {
	case saInstall:
		return saDoInstall(ctx, cmd, mgr, logger, svcName, spec)
	case saUninstall:
		return saDoUninstall(ctx, cmd, mgr, logger, svcName, spec)
	default:
		return saDoStatus(ctx, cmd, mgr, svcName, spec)
	}
}

func saDoStatus(ctx context.Context, cmd *cobra.Command, mgr saManager, name string, spec svcSpec) error {
	s, err := mgr.Status(ctx, spec.label)
	if err != nil && !errors.Is(err, launchd.ErrUnloaded) {
		return fmt.Errorf("setup-agent: status %s: %w", spec.label, err)
	}
	if saQuiet {
		return nil
	}
	state := "not-loaded"
	if s.Loaded {
		state = s.State
		if state == "" {
			state = "loaded"
		}
	}
	if saJSON {
		fmt.Fprintf(cmd.OutOrStdout(),
			`{"service":%q,"label":%q,"loaded":%v,"pid":%q,"state":%q}`+"\n",
			name, spec.label, s.Loaded, s.PID, state)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-34s %s\n", name, spec.label, state)
	return nil
}

func saDoInstall(ctx context.Context, cmd *cobra.Command, mgr saManager, logger *log.Logger, name string, spec svcSpec) error {
	homeDir, err := userHomeDir()
	if err != nil {
		return fmt.Errorf("setup-agent: get home dir: %w", err)
	}
	vars, err := buildSAVars(homeDir, name, spec)
	if err != nil {
		return fmt.Errorf("setup-agent: %w", err)
	}
	plist, err := mgr.RenderTemplate(spec.tmpl, vars)
	if err != nil {
		return fmt.Errorf("setup-agent: render %s: %w", spec.tmpl, err)
	}
	plistPath := launchd.PlistPath(homeDir, spec.label)

	start := time.Now()
	installErr := mgr.Install(ctx, plist, plistPath)
	dur := time.Since(start).Milliseconds()

	saLog(logger, "install:"+name, installErr, dur)

	if installErr != nil {
		return fmt.Errorf("setup-agent: install %s: %w", spec.label, installErr)
	}
	if !saQuiet {
		fmt.Fprintf(cmd.OutOrStdout(), "installed %s → %s\n", spec.label, plistPath)
	}
	return nil
}

func saDoUninstall(ctx context.Context, cmd *cobra.Command, mgr saManager, logger *log.Logger, name string, spec svcSpec) error {
	homeDir, err := userHomeDir()
	if err != nil {
		return fmt.Errorf("setup-agent: get home dir: %w", err)
	}
	plistPath := launchd.PlistPath(homeDir, spec.label)

	start := time.Now()
	uninstErr := mgr.Uninstall(ctx, spec.label, plistPath)
	dur := time.Since(start).Milliseconds()

	saLog(logger, "uninstall:"+name, uninstErr, dur)

	if uninstErr != nil {
		return fmt.Errorf("setup-agent: uninstall %s: %w", spec.label, uninstErr)
	}
	if !saQuiet {
		fmt.Fprintf(cmd.OutOrStdout(), "uninstalled %s\n", spec.label)
	}
	return nil
}

func saLog(logger *log.Logger, action string, opErr error, dur int64) {
	if logger == nil {
		return
	}
	node, _ := os.Hostname()
	result := log.ResultOK
	tail := ""
	if opErr != nil {
		result = log.ResultError
		tail = opErr.Error()
	}
	_ = logger.Action(log.Event{
		Node:       node,
		Phase:      "setup-agent",
		Action:     action,
		Result:     result,
		DurationMs: dur,
		StderrTail: tail,
	})
}

func buildSAVars(homeDir, name string, spec svcSpec) (launchd.Vars, error) {
	vars := launchd.Vars{
		Label:   spec.label,
		HomeDir: homeDir,
	}
	if spec.scriptFn != "" {
		if saRepoDir == "" {
			return vars, fmt.Errorf("--repo-dir is required to install %s", name)
		}
		vars.RepoDir = saRepoDir
		vars.ScriptPath = filepath.Join(saRepoDir, "scripts", spec.scriptFn)
	}
	switch spec.tmpl {
	case launchd.TemplateHeartbeat:
		vars.UserName = saUser
		nl := saNodeLabel
		if nl == "" {
			h, err := os.Hostname()
			if err == nil {
				nl = strings.SplitN(h, ".", 2)[0]
			} else {
				nl = "unknown"
			}
		}
		vars.NodeLabel = nl
	case launchd.TemplateBackup:
		vars.ResticRepository = saResticRepo
		vars.ResticPasswordCommand = saResticPassCmd
	case launchd.TemplateWatchdog:
		vars.AlertURL = saAlertURL
	}
	return vars, nil
}

func sortedSvcNames() []string {
	names := make([]string, 0, len(knownSvcs))
	for k := range knownSvcs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func setupAgentSkill() Skill {
	return Skill{
		Name:    "setup-agent",
		Trigger: "/setup-agent",
		Summary: "Install, uninstall, or report status of a com.hermes.* LaunchAgent on the gateway.",
		Args: []SkillArg{
			{Name: "<service>", Description: "Service: heartbeat, backup, backup-worker, watchdog, or logrotate."},
			{Name: "--install", Description: "Install and load the LaunchAgent plist."},
			{Name: "--uninstall", Description: "Unload and remove the LaunchAgent plist."},
			{Name: "--repo-dir PATH", Description: "hermes-setup repo root (required for --install of script-based agents)."},
			{Name: "--hermes-dir PATH", Description: "Override ~/.hermes for the JSONL log path."},
			{Name: "--alert-url URL", Description: "ntfy alert URL (watchdog --install)."},
			{Name: "--restic-repository REPO", Description: "Restic repo URI (backup --install)."},
			{Name: "--restic-password-command CMD", Description: "Restic password command (backup --install)."},
			{Name: "--user NAME", Description: "Worker username for heartbeat plist (default: hermes)."},
			{Name: "--node-label LABEL", Description: "Heartbeat node label (default: hostname)."},
			{Name: "--json", Description: "Emit JSON result to stdout."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
		},
		Examples: []SkillExample{
			{Description: "Check heartbeat status", Command: "fleetctl setup-agent heartbeat"},
			{Description: "Install backup agent", Command: "fleetctl setup-agent backup --install --repo-dir ~/hermes-setup"},
			{Description: "Uninstall watchdog", Command: "fleetctl setup-agent watchdog --uninstall"},
		},
	}
}

func init() { registerSkill("setup-agent", setupAgentSkill) }
