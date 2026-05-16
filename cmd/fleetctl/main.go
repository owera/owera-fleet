// Command fleetctl is the single binary that operates the Owera fleet.
//
// Each fleet capability — bootstrap, delegate, health, audit, scenario
// runs, skill generation, state regeneration — is a cobra subcommand under
// this binary, replacing the 23-script bash surface of hermes-setup with a
// typed, discoverable, tab-completable dispatcher.
//
// The binary itself is a thin entrypoint: it wires the subcommands and
// returns. All behaviour lives under cmd/fleetctl/commands and the typed
// internal/ packages.
package main

import (
	"fmt"
	"os"

	"github.com/owera/owera-fleet/cmd/fleetctl/commands"
)

func main() {
	if err := commands.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "fleetctl: %v\n", err)
		os.Exit(1)
	}
}
