// smoke.go is the minimal worker-reachability probe required to verify
// T2.1 of WS-2 (internal/ssh) end-to-end against the live fleet.
//
// This is *not* the full smoke-test surface that hermes-setup ships in
// smoke-test-node.sh — that richer set (binary presence, hermes version,
// heartbeat age) lands later under WS-4 T4.3. For now, the command:
//
//  1. Resolves the target via either an explicit arg or ~/.hermes/nodes.txt.
//  2. Dials via [internal/ssh] (Keychain-backed agent auth).
//  3. Runs `whoami; uname -srm`.
//  4. Prints PASS / FAIL with exit codes 0 / 1.
package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/nodes"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

var (
	smokeNode       string
	smokeKnownHosts string
	smokeStrict     bool
	smokeTimeout    time.Duration
)

var smokeCmd = &cobra.Command{
	Use:   "smoke [user@host]",
	Short: "Probe worker reachability via SSH agent auth",
	Long: `smoke opens an SSH session to a worker and runs a tiny "whoami; uname"
probe. Used to verify the gateway can reach the worker over the dedicated
key (Keychain-backed agent) before delegating real work.

If no target is supplied as an arg, falls back to --node, which itself
falls back to the first entry in ~/.hermes/nodes.txt. Acceptance for
T2.1: "fleetctl smoke hermes@claw1.local" prints PASS.`,
	Example: `  fleetctl smoke hermes@claw1.local
  fleetctl smoke --node hermes@claw2.local
  fleetctl smoke --strict-host-key            # require known_hosts entry`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSmoke,
}

func init() {
	smokeCmd.Flags().StringVar(&smokeNode, "node", "", "target user@host (overrides nodes.txt; positional arg wins)")
	smokeCmd.Flags().StringVar(&smokeKnownHosts, "known-hosts", "", "override ~/.ssh/known_hosts")
	smokeCmd.Flags().BoolVar(&smokeStrict, "strict-host-key", false, "reject hosts not already in known_hosts")
	smokeCmd.Flags().DurationVar(&smokeTimeout, "timeout", 15*time.Second, "overall probe timeout")
	rootCmd.AddCommand(smokeCmd)
}

func runSmoke(cmd *cobra.Command, args []string) error {
	target, err := resolveSmokeTarget(args)
	if err != nil {
		return err
	}

	dialerOpts := []osh.Option{}
	if smokeKnownHosts != "" {
		dialerOpts = append(dialerOpts, osh.WithKnownHostsPath(smokeKnownHosts))
	}
	if smokeStrict {
		dialerOpts = append(dialerOpts, osh.WithStrictHostKey())
	}
	dialerOpts = append(dialerOpts, osh.WithConnectTimeout(smokeTimeout))

	ctx, cancel := context.WithTimeout(cmd.Context(), smokeTimeout)
	defer cancel()

	dialer := osh.NewDialer(dialerOpts...)
	client, err := dialer.Dial(ctx, target)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "FAIL %s — dial: %v\n", target, err)
		return errors.New("smoke: dial failed")
	}
	defer client.Close()

	out, err := client.RunCapture(ctx, "whoami; uname -srm")
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "FAIL %s — run: %v\n", target, err)
		return errors.New("smoke: probe failed")
	}

	fmt.Fprintf(cmd.OutOrStdout(), "PASS %s\n", target)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", line)
	}
	return nil
}

// resolveSmokeTarget honors the priority: positional arg > --node flag >
// first entry in nodes.txt. Empty all three is an error.
func resolveSmokeTarget(args []string) (osh.Target, error) {
	raw := ""
	switch {
	case len(args) == 1 && strings.TrimSpace(args[0]) != "":
		raw = args[0]
	case smokeNode != "":
		raw = smokeNode
	default:
		reg, err := nodes.Load("")
		if err != nil {
			return osh.Target{}, fmt.Errorf("smoke: no target supplied and could not read nodes.txt: %w", err)
		}
		all := reg.All()
		if len(all) == 0 {
			return osh.Target{}, errors.New("smoke: no target supplied and nodes.txt is empty")
		}
		raw = all[0].String()
	}
	return osh.ParseTarget(raw)
}

func smokeSkill() Skill {
	return Skill{
		Name:    "smoke",
		Trigger: "/smoke",
		Summary: "Probe a worker's SSH reachability via the gateway's Keychain-backed agent key.",
		Args: []SkillArg{
			{Name: "user@host (positional)", Description: "Explicit target; overrides --node and nodes.txt."},
			{Name: "--node USER@HOST", Description: "Target override when no positional arg."},
			{Name: "--known-hosts PATH", Description: "Use an alternate known_hosts file."},
			{Name: "--strict-host-key", Description: "Refuse to accept new host keys; require an existing entry."},
			{Name: "--timeout DUR", Description: "Overall probe timeout (default 15s)."},
		},
		Examples: []SkillExample{
			{Description: "Probe a specific worker", Command: "fleetctl smoke hermes@claw1.local"},
			{Description: "Probe the first node in nodes.txt", Command: "fleetctl smoke"},
		},
	}
}

func init() { registerSkill("smoke", smokeSkill) }
