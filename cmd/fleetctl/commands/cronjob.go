// cronjob.go implements `fleetctl cronjob` — CRUD over Hermes-managed cron
// jobs (NOT launchd). Used for long-running pollers like the §3.3 ticket
// triage watcher.
package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/hermesjobs"
)

var (
	cjName     string
	cjSchedule string
	cjCommand  string
	cjEnv      []string
	cjTags     []string
	cjJSON     bool
	cjDryRun   bool
)

var cronjobCmd = &cobra.Command{
	Use:   "cronjob",
	Short: "Manage Hermes-managed cron jobs (separate from launchd)",
	Long: `cronjob is the typed wrapper around 'hermes cronjob' — schedules and
removes long-running pollers that run inside the Hermes runtime.

Distinct from setup-agent (which manages launchd LaunchAgents): hermesjobs
share Hermes' sandbox + secrets and are best for AI/agent work.`,
}

var cjListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Hermes cron jobs",
	RunE:  runCronjobList,
}

var cjInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install or update a Hermes cron job (idempotent)",
	RunE:  runCronjobInstall,
}

var cjRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a Hermes cron job (idempotent)",
	Args:  cobra.ExactArgs(1),
	RunE:  runCronjobRemove,
}

var cjStatusCmd = &cobra.Command{
	Use:   "status <name>",
	Short: "Show the current state of one Hermes cron job",
	Args:  cobra.ExactArgs(1),
	RunE:  runCronjobStatus,
}

// cronjobManager is overridable in tests.
var cronjobManager = func() *hermesjobs.Manager { return hermesjobs.New() }

func init() {
	cronjobCmd.PersistentFlags().BoolVar(&cjJSON, "json", false, "emit JSON output")
	cjInstallCmd.Flags().StringVar(&cjName, "name", "", "job name (required)")
	cjInstallCmd.Flags().StringVar(&cjSchedule, "schedule", "", "cron schedule (required)")
	cjInstallCmd.Flags().StringVar(&cjCommand, "command", "", "command Hermes should run (required)")
	cjInstallCmd.Flags().StringSliceVar(&cjEnv, "env", nil, "env var KEY=VALUE (repeatable)")
	cjInstallCmd.Flags().StringSliceVar(&cjTags, "tag", nil, "tag (repeatable)")
	cjInstallCmd.Flags().BoolVar(&cjDryRun, "dry-run", false, "print the resolved Job spec without calling hermes cronjob")

	cronjobCmd.AddCommand(cjListCmd, cjInstallCmd, cjRemoveCmd, cjStatusCmd)
	rootCmd.AddCommand(cronjobCmd)
}

func runCronjobList(cmd *cobra.Command, _ []string) error {
	mgr := cronjobManager()
	jobs, err := mgr.List(cmd.Context())
	if err != nil {
		return err
	}
	if cjJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(jobs)
	}
	if len(jobs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no cron jobs)")
		return nil
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%-24s %-16s %-8s %s\n", "Name", "Schedule", "Enabled", "Command")
	fmt.Fprintln(w, strings.Repeat("-", 100))
	for _, j := range jobs {
		fmt.Fprintf(w, "%-24s %-16s %-8v %s\n", j.Name, j.Schedule, j.Enabled, truncateAction(j.Command))
	}
	return nil
}

func runCronjobInstall(cmd *cobra.Command, _ []string) error {
	if cjName == "" || cjSchedule == "" || cjCommand == "" {
		return errors.New("cronjob install: --name, --schedule, and --command are required")
	}
	env := make(map[string]string)
	for _, kv := range cjEnv {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			return fmt.Errorf("cronjob install: invalid --env %q (want KEY=VALUE)", kv)
		}
		env[kv[:idx]] = kv[idx+1:]
	}
	job := hermesjobs.Job{
		Name:     cjName,
		Schedule: cjSchedule,
		Command:  cjCommand,
		Env:      env,
		Tags:     cjTags,
		Enabled:  true,
	}
	if cjDryRun {
		w := cmd.OutOrStdout()
		if cjJSON {
			return json.NewEncoder(w).Encode(job)
		}
		fmt.Fprintf(w, "DRY-RUN: would install cronjob\n")
		fmt.Fprintf(w, "  Name:     %s\n", job.Name)
		fmt.Fprintf(w, "  Schedule: %s\n", job.Schedule)
		fmt.Fprintf(w, "  Command:  %s\n", job.Command)
		if len(job.Env) > 0 {
			fmt.Fprintf(w, "  Env:      %v\n", job.Env)
		}
		if len(job.Tags) > 0 {
			fmt.Fprintf(w, "  Tags:     %v\n", job.Tags)
		}
		return nil
	}
	mgr := cronjobManager()
	created, err := mgr.Install(cmd.Context(), job)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(cmd.OutOrStdout(), "Installed cronjob %s\n", cjName)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Cronjob %s is already up to date (no-change)\n", cjName)
	}
	return nil
}

func runCronjobRemove(cmd *cobra.Command, args []string) error {
	mgr := cronjobManager()
	if err := mgr.Remove(cmd.Context(), args[0]); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed cronjob %s\n", args[0])
	return nil
}

func runCronjobStatus(cmd *cobra.Command, args []string) error {
	mgr := cronjobManager()
	j, err := mgr.Status(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	if cjJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(j)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Cronjob %s\n", j.Name)
	fmt.Fprintf(w, "  Schedule: %s\n", j.Schedule)
	fmt.Fprintf(w, "  Command:  %s\n", j.Command)
	fmt.Fprintf(w, "  Enabled:  %v\n", j.Enabled)
	if !j.LastRun.IsZero() {
		fmt.Fprintf(w, "  LastRun:  %s (exit=%d)\n", j.LastRun, j.LastExit)
	}
	return nil
}

// Reserved for future tests that drive cronjob through a fake manager.
var _ = context.Background

func cronjobSkill() Skill {
	return Skill{
		Name:    "cronjob",
		Trigger: "/cronjob",
		Summary: "Manage Hermes-managed cron jobs (long-running pollers run inside the Hermes runtime).",
		Args: []SkillArg{
			{Name: "list", Description: "List all Hermes cron jobs."},
			{Name: "install --name N --schedule S --command C", Description: "Install or update a job."},
			{Name: "remove <name>", Description: "Remove a job by name."},
			{Name: "status <name>", Description: "Show a job's state + last run."},
			{Name: "--env KEY=VALUE", Description: "Per-job env var (repeatable)."},
			{Name: "--tag T", Description: "Per-job tag (repeatable)."},
			{Name: "--dry-run", Description: "Print the resolved Job spec (install only) without calling hermes cronjob."},
		},
		Examples: []SkillExample{
			{Description: "Install ticket triage poller", Command: "fleetctl cronjob install --name triage --schedule '*/5 * * * *' --command 'hermes z \"triage ticket queue\"'"},
			{Description: "Preview the install plan", Command: "fleetctl cronjob install --name triage --schedule '*/5 * * * *' --command 'echo hi' --dry-run"},
			{Description: "List jobs as JSON", Command: "fleetctl cronjob list --json"},
		},
	}
}

func init() { registerSkill("cronjob", cronjobSkill) }
