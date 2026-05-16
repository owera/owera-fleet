// Package commands assembles the cobra dispatcher for fleetctl.
//
// Each subcommand lives in its own file. New subcommands register
// themselves with the root command via init() so adding a command is a
// single-file PR. The root command itself stays tiny — just version,
// global flags (none for now), and the registered children.
package commands

import (
	"github.com/spf13/cobra"
)

// Version is overwritten at link time by the release pipeline:
//
//	go build -ldflags="-X github.com/owera/owera-fleet/cmd/fleetctl/commands.Version=v0.1.0"
//
// Defaults to "dev" so a hand-built binary tells operators it is unstamped.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "fleetctl",
	Short: "Owera fleet operator-plane CLI",
	Long: `fleetctl is the operator-plane CLI for the Owera Hermes fleet.

It is the typed single-binary successor to the hermes-setup bash codebase.
Subcommands operate on the gateway-resident state under ~/.hermes/ and on
the workers reachable from ~/.hermes/nodes.txt.

Run "fleetctl <command> --help" for details on each subcommand.`,
	Version:       Version,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the dispatcher. Returns the cobra-derived error (if any) so
// main can pick the exit code. Callers in tests can use rootCmd directly.
func Execute() error {
	return rootCmd.Execute()
}

// Root exposes the root cobra command for tests and for the gen-skills
// command (which walks the registered subcommands to emit SKILL.md files).
func Root() *cobra.Command {
	return rootCmd
}
