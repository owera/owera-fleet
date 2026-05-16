// state.go implements `fleetctl state` — the typed regeneration of what
// hermes-setup's hand-edited STATE.md used to record. It collects the
// live ~/.hermes/ snapshot (pinned version, last backup, nodes, launchd
// agents, config summary, JSONL log roll-up) and emits markdown or JSON.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/state"
)

var (
	stateOutputJSON bool
	stateOutputPath string
	stateHermesDir  string
)

var stateCmd = &cobra.Command{
	Use:   "state",
	Short: "Regenerate the operational-state snapshot (replaces hand-edited STATE.md)",
	Long: `state collects a point-in-time snapshot of the live Hermes gateway and
emits it as markdown (default) or JSON.

It reads ~/.hermes/{PINNED_VERSION, PINNED_VERSION.prev, LAST_BACKUP,
nodes.txt, config.yaml}, shells out to launchctl print to list the
com.hermes.* user-domain services, and summarises every JSONL log file
under ~/.hermes/logs/. Missing files are reported as empty rather than
fatal so the snapshot still renders if a node has not yet seeded one.

Acceptance for T1.5: "fleetctl state --markdown" is equivalent to the
hand-maintained STATE.md (timestamps differ).`,
	Example: `  fleetctl state                    # markdown to stdout
  fleetctl state --json             # JSON to stdout
  fleetctl state --out STATE.md     # regenerate STATE.md in-place
  fleetctl state --hermes-dir /tmp  # collect against a test directory`,
	RunE: runState,
}

func init() {
	stateCmd.Flags().BoolVar(&stateOutputJSON, "json", false, "emit JSON instead of markdown")
	stateCmd.Flags().BoolVar(new(bool), "markdown", true, "emit markdown (default); kept for explicit operator scripts")
	stateCmd.Flags().StringVar(&stateOutputPath, "out", "", "write to this path instead of stdout")
	stateCmd.Flags().StringVar(&stateHermesDir, "hermes-dir", "", "override ~/.hermes/ (test/staging)")
	rootCmd.AddCommand(stateCmd)
}

func runState(cmd *cobra.Command, _ []string) error {
	collector := state.Default(stateHermesDir)
	snap, err := collector.Collect(context.Background())
	if err != nil {
		if errors.Is(err, state.ErrNoHome) {
			return fmt.Errorf("could not resolve ~/.hermes/: %w (set HOME or pass --hermes-dir)", err)
		}
		return err
	}

	var data []byte
	if stateOutputJSON {
		data, err = snap.JSON()
		if err != nil {
			return fmt.Errorf("render JSON: %w", err)
		}
		data = append(data, '\n')
	} else {
		data = snap.Markdown()
	}

	if stateOutputPath == "" {
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}
	return os.WriteFile(stateOutputPath, data, 0o644)
}

// stateSkill is consumed by `fleetctl gen-skills` (T1.6). Declared here so
// the command's metadata lives next to the implementation; the gen-skills
// command walks rootCmd's children, looks up the registered Skill spec by
// command name, and renders SKILL.md from it.
func stateSkill() Skill {
	return Skill{
		Name:    "state",
		Trigger: "/state",
		Summary: "Regenerate ~/.hermes/ operational-state snapshot as markdown or JSON.",
		Args: []SkillArg{
			{Name: "--json", Description: "Emit JSON instead of markdown."},
			{Name: "--out PATH", Description: "Write to PATH instead of stdout."},
			{Name: "--hermes-dir DIR", Description: "Override ~/.hermes/ (test/staging)."},
		},
		Examples: []SkillExample{
			{Description: "Regenerate STATE.md in-place", Command: "fleetctl state --out STATE.md"},
			{Description: "Emit JSON for a dashboard", Command: "fleetctl state --json"},
		},
	}
}

func init() { registerSkill("state", stateSkill) }
