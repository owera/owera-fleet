// verify.go implements `fleetctl verify` — the WS-12 Phase-1 verification
// gate. Runs every internal check (build / vet / test / skill-drift +
// six in-process self-tests) and emits a pass/fail report.
//
// The live-fleet QVE gate that exercises real hardware lives in the
// hermes-setup repo. This command is the half that can run in CI on every
// PR: deterministic, hermetic, no SSH, no remote nodes.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	flog "github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/verify"
)

var (
	verifyJSON       bool
	verifyCheckIDs   []string
	verifyRepo       string
	verifyMaxPar     int
	verifyLogPath    = "~/.hermes/logs/verify.jsonl"
	verifyHermesDir  string
	verifyQuiet      bool
)

var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Run the WS-12 Phase-1 verification gate",
	Long: `verify runs the Phase-1 verification gate: every internal check the
project needs to pass before a release is shippable.

The gate covers four shell-out checks (go build, go vet, go test -race,
fleetctl gen-skills --check) and six in-process self-tests (ledger,
markers, pairing, budget, configsync, metrics). The full live-fleet QVE
gate (real workers, real SSH, real Hermes) is a separate runbook in the
hermes-setup repo; this command is the CI-friendly subset.

A non-zero exit indicates at least one required check failed. Optional
checks failing are surfaced in the report but do not gate the exit.`,
	Example: `  fleetctl verify
  fleetctl verify --json
  fleetctl verify --check go_test
  fleetctl verify --repo /path/to/owera-fleet`,
	RunE: runVerify,
}

func init() {
	verifyCmd.Flags().BoolVar(&verifyJSON, "json", false, "emit the report as JSON instead of a text table")
	verifyCmd.Flags().StringSliceVar(&verifyCheckIDs, "check", nil, "only run check(s) with this ID (repeatable)")
	verifyCmd.Flags().StringVar(&verifyRepo, "repo", "", "repo root for shell-out checks (default: walk up from cwd to go.mod)")
	verifyCmd.Flags().IntVar(&verifyMaxPar, "parallel", 4, "maximum number of checks to run concurrently")
	verifyCmd.Flags().StringVar(&verifyHermesDir, "hermes-dir", "", "override ~/.hermes path (also redirects the JSONL log)")
	verifyCmd.Flags().BoolVar(&verifyQuiet, "quiet", false, "suppress text output (still emits JSONL log + exit code)")
	rootCmd.AddCommand(verifyCmd)
}

func runVerify(cmd *cobra.Command, _ []string) error {
	checks := verify.DefaultChecks(verifyRepo)
	checks = verify.FilterByIDs(checks, verifyCheckIDs)
	if len(checks) == 0 {
		return fmt.Errorf("verify: no checks selected (bad --check ID?)")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	report := verify.RunAll(ctx, checks, verifyMaxPar)

	// Emit JSONL audit event first — we want the audit trail even if
	// the renderer fails for some reason.
	logVerifyGate(cmd, report)

	if verifyJSON {
		if !verifyQuiet {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(report); err != nil {
				return fmt.Errorf("verify: encode json: %w", err)
			}
		}
	} else if !verifyQuiet {
		renderVerifyTable(cmd, report)
	}

	if !report.OK {
		// Same convention as `delegate`: silence cobra's "Error: …"
		// wrapper since the per-check detail already told the operator
		// what went wrong.
		cmd.SilenceUsage = true
		return fmt.Errorf("verify: gate failed (%d required check(s) failed)", report.Failed)
	}
	return nil
}

func renderVerifyTable(cmd *cobra.Command, r verify.Report) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%-18s %-32s %-6s %-10s %s\n", "ID", "Name", "Status", "Duration", "Evidence")
	fmt.Fprintln(w, strings.Repeat("-", 100))
	for _, res := range r.Results {
		status := string(res.Status)
		if res.Optional && res.Status == verify.StatusFail {
			status = "fail*"
		}
		ev := res.Evidence
		if res.Status == verify.StatusFail && res.ErrMsg != "" {
			ev = res.ErrMsg
		}
		if len(ev) > 50 {
			ev = ev[:47] + "..."
		}
		name := res.Name
		if len(name) > 32 {
			name = name[:29] + "..."
		}
		fmt.Fprintf(w, "%-18s %-32s %-6s %-10s %s\n",
			res.ID, name, status, res.Duration.Round(time.Millisecond), ev)
	}
	fmt.Fprintln(w)
	verdict := "PASS"
	if !r.OK {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "%s: %d passed, %d failed, %d skipped in %s\n",
		verdict, r.Passed, r.Failed, r.Skipped, r.Elapsed().Round(time.Millisecond))
	if !r.OK {
		fmt.Fprintln(w, "(fail* = optional check; does not gate the exit code)")
	}
}

// logVerifyGate writes one JSONL event describing the gate run to
// ~/.hermes/logs/verify.jsonl. The event reflects the overall gate
// verdict, not per-check granularity (those live in the text/JSON
// report). Best-effort: failure to open the log file is reported to
// stderr but does not gate the verify exit code.
func logVerifyGate(cmd *cobra.Command, r verify.Report) {
	logSrc := verifyLogPath
	if verifyHermesDir != "" {
		logSrc = verifyHermesDir + "/logs/verify.jsonl"
	}
	resolved, err := expandHomePath(logSrc)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "verify: expand log path: %v\n", err)
		return
	}
	logger, err := openLogger(resolved)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "verify: open log %s: %v\n", resolved, err)
		return
	}
	defer logger.Close()

	host, _ := os.Hostname()
	result := flog.ResultOK
	stderrTail := ""
	if !r.OK {
		result = flog.ResultError
		stderrTail = summarizeFailures(r)
	}
	event := flog.Event{
		Node:       host,
		Phase:      "verify",
		Action:     "gate",
		Result:     result,
		DurationMs: r.Elapsed().Milliseconds(),
		StderrTail: stderrTail,
	}
	if err := logger.Action(event); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "verify: write event: %v\n", err)
	}
}

func summarizeFailures(r verify.Report) string {
	var b strings.Builder
	for _, res := range r.Results {
		if res.Status != verify.StatusFail {
			continue
		}
		tag := res.ID
		if res.Optional {
			tag += "(optional)"
		}
		fmt.Fprintf(&b, "%s: %s\n", tag, res.ErrMsg)
	}
	out := b.String()
	if len(out) > flog.MaxStderrTailBytes {
		out = out[len(out)-flog.MaxStderrTailBytes:]
	}
	return out
}

func verifySkill() Skill {
	return Skill{
		Name:    "verify",
		Trigger: "/verify",
		Summary: "Run the WS-12 Phase-1 verification gate: build / vet / test / skills-drift + six self-tests.",
		Args: []SkillArg{
			{Name: "--json", Description: "Emit the report as JSON instead of a text table."},
			{Name: "--check ID", Description: "Only run check(s) with this ID (repeatable: --check go_test --check ledger_self)."},
			{Name: "--repo PATH", Description: "Override repo root (default: walk up from cwd looking for go.mod)."},
			{Name: "--parallel N", Description: "Maximum concurrent checks (default 4)."},
			{Name: "--hermes-dir PATH", Description: "Override ~/.hermes path (also redirects the JSONL log)."},
			{Name: "--quiet", Description: "Suppress text/JSON output; still emits the audit event and exit code."},
		},
		Examples: []SkillExample{
			{Description: "Run the full gate", Command: "fleetctl verify"},
			{Description: "Run only the test phase", Command: "fleetctl verify --check go_test"},
			{Description: "Machine-readable report", Command: "fleetctl verify --json"},
		},
	}
}

func init() { registerSkill("verify", verifySkill) }
