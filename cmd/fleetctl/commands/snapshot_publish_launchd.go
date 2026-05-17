// snapshot_publish_launchd.go — install / uninstall / status subcommands
// for `fleetctl snapshot-publish`. Wraps the binary in a user-domain
// LaunchAgent so it survives reboots + restarts on crash.
//
// The plist lives at ~/Library/LaunchAgents/com.owera.snapshot-publish.plist;
// install renders the bundled template, writes it, and runs
// `launchctl bootstrap gui/$UID <path>`. Uninstall runs `launchctl bootout`
// and removes the file. Status calls `launchctl print` and pretty-prints
// the relevant fields.
package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/launchd"
)

const (
	// snapshotPublishLabel is the launchd Label for the agent.
	snapshotPublishLabel = "com.owera.snapshot-publish"

	// snapshotPublishTemplateName is the basename (no `.plist.tmpl`) of
	// the bundled template that produces the plist.
	snapshotPublishTemplateName = "snapshot-publish"
)

// snapshotPublishPlistPath returns the absolute path the launchd plist
// is installed to. Hard-coded to ~/Library/LaunchAgents/<Label>.plist
// — the canonical user-domain location.
func snapshotPublishPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", snapshotPublishLabel+".plist"), nil
}

// fleetctlBinaryPath returns the absolute path of the currently-running
// fleetctl binary, baked into the rendered plist so launchd invokes the
// exact build the operator installed.
func fleetctlBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("abs(%s): %w", exe, err)
	}
	return abs, nil
}

// newLaunchdManagerFn lets tests substitute a Manager that mocks
// RunCmd / WriteFile / etc. without touching the real launchctl.
var newLaunchdManagerFn = func() *launchd.Manager { return launchd.New(nil) }

// --- install ---

var snapshotPublishInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install com.owera.snapshot-publish as a user LaunchAgent",
	Long: `install renders the bundled snapshot-publish.plist template against
the current fleetctl binary path and operator $HOME, writes it to
~/Library/LaunchAgents/com.owera.snapshot-publish.plist, then runs
launchctl bootstrap to activate it.

Idempotent — if the agent is already loaded with the same plist the
underlying launchctl returns "service already loaded" and exits non-
zero; the operator should run uninstall+install (or 'launchctl
kickstart') to apply changes.`,
	RunE: runSnapshotPublishInstall,
}

func runSnapshotPublishInstall(cmd *cobra.Command, _ []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("install: home dir: %w", err)
	}
	exe, err := fleetctlBinaryPath()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	plistPath, err := snapshotPublishPlistPath()
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}

	m := newLaunchdManagerFn()
	body, err := m.RenderTemplate(snapshotPublishTemplateName, launchd.Vars{
		Label:      snapshotPublishLabel,
		UID:        os.Getuid(),
		HomeDir:    home,
		ScriptPath: exe,
	})
	if err != nil {
		return fmt.Errorf("install: render: %w", err)
	}
	if err := m.Install(cmd.Context(), body, plistPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"snapshot-publish: installed at %s (label=%s, exe=%s)\n",
		plistPath, snapshotPublishLabel, exe)
	return nil
}

// --- uninstall ---

var snapshotPublishUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Tear down the com.owera.snapshot-publish LaunchAgent",
	Long: `uninstall runs `+"`launchctl bootout gui/$UID/com.owera.snapshot-publish`"+`
and removes the plist. Idempotent — if the label isn't loaded the
bootout step logs a warning but does not error, so calling uninstall
twice (or before install) is safe.`,
	RunE: runSnapshotPublishUninstall,
}

func runSnapshotPublishUninstall(cmd *cobra.Command, _ []string) error {
	plistPath, err := snapshotPublishPlistPath()
	if err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	m := newLaunchdManagerFn()
	if err := m.Uninstall(cmd.Context(), snapshotPublishLabel, plistPath); err != nil {
		return fmt.Errorf("uninstall: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"snapshot-publish: uninstalled (%s, label=%s)\n",
		plistPath, snapshotPublishLabel)
	return nil
}

// --- status ---

var snapshotPublishStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print the running state of com.owera.snapshot-publish",
	Long: `status runs `+"`launchctl print gui/$UID/com.owera.snapshot-publish`"+`
and pretty-prints PID, last exit code, and loaded/unloaded state. Exits
non-zero if the label is not loaded (an operator-friendly probe).`,
	RunE: runSnapshotPublishStatus,
}

func runSnapshotPublishStatus(cmd *cobra.Command, _ []string) error {
	m := newLaunchdManagerFn()
	st, err := m.Status(cmd.Context(), snapshotPublishLabel)
	if err != nil {
		if errors.Is(err, launchd.ErrUnloaded) {
			fmt.Fprintf(cmd.OutOrStdout(), "snapshot-publish: not loaded (label=%s)\n", snapshotPublishLabel)
			return fmt.Errorf("not loaded")
		}
		return fmt.Errorf("status: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"snapshot-publish: label=%s loaded=%t pid=%s last_exit=%s state=%s\n",
		st.Label, st.Loaded, st.PID, st.LastExitCode, st.State)
	return nil
}

func init() {
	snapshotPublishCmd.AddCommand(snapshotPublishInstallCmd)
	snapshotPublishCmd.AddCommand(snapshotPublishUninstallCmd)
	snapshotPublishCmd.AddCommand(snapshotPublishStatusCmd)
}
