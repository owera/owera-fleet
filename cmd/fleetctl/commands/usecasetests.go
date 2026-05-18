// usecasetests.go implements `fleetctl usecase-tests` — the operator-shaped
// end-to-end test pass that supersedes hermes-setup/scripts/fleet-usecase-tests.sh.
//
// # Background
//
// hermes-setup era: fleet-usecase-tests.sh wired 10 operator scenarios
// (S1..S10) by shelling out to delegate-to-node.sh, review-branch.sh,
// research-with-hermes.sh, fleet-health-snapshot.sh, etc. Each scenario
// emitted a markdown section + a JSONL audit line. Bash 3.2 + sed + grep
// glue.
//
// owera-fleet era: this subcommand calls the Go primitives in
// internal/usecasetests directly — no subprocess spawning, no bash
// dependency. Same 10 scenarios in the same order with the same exit
// contract: 0 if no FAIL, 1 otherwise.
//
// # Output
//
// Default output is a markdown report on stdout (with a summary table at
// the top followed by per-scenario detail sections). --format json
// dumps the Summary struct verbatim for piping into jq / a metrics
// pipeline. Either way, every scenario also appends one JSONL record
// to ~/.hermes/logs/usecase-tests.jsonl with the canonical
// {ts, node, phase, action, result, duration_ms, stderr_tail} schema.

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/usecasetests"
)

var (
	usecaseTestsScenario       []string
	usecaseTestsFormat         string
	usecaseTestsAll            bool
	usecaseTestsContinueOnFail bool
	usecaseTestsSkipLLM        bool
	usecaseTestsFreeModel      string
	usecaseTestsReviewRepo     string
	usecaseTestsReviewBase     string
	usecaseTestsResearchTopic  string
	usecaseTestsHermesDir      string
	usecaseTestsTimeout        time.Duration
	usecaseTestsQuiet          bool
	usecaseTestsLogPath        = "~/.hermes/logs/usecase-tests.jsonl"
)

// newUsecaseTestsRunner is overridable in tests so the cobra wrapper
// can drive a mock runner with deterministic scenarios.
var newUsecaseTestsRunner = func() *usecasetests.Runner {
	return &usecasetests.Runner{Scenarios: usecasetests.DefaultScenarios()}
}

var usecaseTestsCmd = &cobra.Command{
	Use:   "usecase-tests",
	Short: "Run the operator-shaped end-to-end test pass (replaces fleet-usecase-tests.sh)",
	Long: `usecase-tests runs the 10 operator-shaped scenarios that exercise the use
cases the v2 deployment plan promises: delegation (S1, S2), review (S3),
research (S4), observation (S5-S8), lifecycle (S9), resilience (S10).

Each scenario calls into the relevant fleetctl internal package directly
(no subprocess to a bash script). Output is a markdown report by default;
--format json emits the Summary struct.

Default behaviour stops at the first FAIL — pass --continue-on-fail to
run every selected scenario regardless. WARN and SKIP outcomes do NOT
affect the exit code: 0 iff no scenario FAILed, 1 otherwise.

Replaces: hermes-setup/scripts/fleet-usecase-tests.sh.`,
	Example: `  fleetctl usecase-tests
  fleetctl usecase-tests --scenario S1,S5,S10
  fleetctl usecase-tests --skip-llm --continue-on-fail
  fleetctl usecase-tests --format json`,
	RunE: runUsecaseTests,
}

func init() {
	usecaseTestsCmd.Flags().StringSliceVar(&usecaseTestsScenario, "scenario", nil, "comma-separated scenario IDs (S1..S10); empty = all")
	usecaseTestsCmd.Flags().StringVar(&usecaseTestsFormat, "format", "markdown", "output format: markdown|json")
	usecaseTestsCmd.Flags().BoolVar(&usecaseTestsAll, "all", true, "run every scenario (default; --scenario takes precedence)")
	usecaseTestsCmd.Flags().BoolVar(&usecaseTestsContinueOnFail, "continue-on-fail", false, "keep running after a FAIL")
	usecaseTestsCmd.Flags().BoolVar(&usecaseTestsSkipLLM, "skip-llm", false, "skip S4 and run S3 in structural-only mode (no Hermes calls)")
	usecaseTestsCmd.Flags().StringVar(&usecaseTestsFreeModel, "free-model", usecasetests.DefaultFreeModel, "OpenRouter free-tier model for S3/S4")
	usecaseTestsCmd.Flags().StringVar(&usecaseTestsReviewRepo, "review-repo", "", "repo path for S3 (default: cwd)")
	usecaseTestsCmd.Flags().StringVar(&usecaseTestsReviewBase, "review-base", usecasetests.DefaultReviewBase, "base ref for S3 diff")
	usecaseTestsCmd.Flags().StringVar(&usecaseTestsResearchTopic, "research-topic", usecasetests.DefaultResearchTopic, "topic for S4 research query")
	usecaseTestsCmd.Flags().StringVar(&usecaseTestsHermesDir, "hermes-dir", "", "override ~/.hermes (used by S5, S9, S10)")
	usecaseTestsCmd.Flags().DurationVar(&usecaseTestsTimeout, "timeout", 5*time.Minute, "per-scenario wall-clock cap")
	usecaseTestsCmd.Flags().BoolVar(&usecaseTestsQuiet, "quiet", false, "suppress stdout; JSONL log unchanged")
	rootCmd.AddCommand(usecaseTestsCmd)
	registerSkill("usecase-tests", usecaseTestsSkill)
}

func runUsecaseTests(cmd *cobra.Command, _ []string) error {
	format := strings.ToLower(strings.TrimSpace(usecaseTestsFormat))
	switch format {
	case "", "markdown":
		format = "markdown"
	case "json":
	default:
		return fmt.Errorf("usecase-tests: --format must be markdown or json (got %q)", usecaseTestsFormat)
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// --all is an explicit override that clears --scenario. When both
	// are set the user is contradicting themselves; we honour --all
	// (since it was set explicitly to true) and surface the conflict
	// on stderr so the operator notices.
	scenarioFilter := usecaseTestsScenario
	if usecaseTestsAll && len(scenarioFilter) > 0 && cmd.Flags().Changed("all") {
		fmt.Fprintln(cmd.ErrOrStderr(), "usecase-tests: --all set; ignoring --scenario filter")
		scenarioFilter = nil
	}

	runner := newUsecaseTestsRunner()
	runner.ContinueOnFail = usecaseTestsContinueOnFail

	opts := usecasetests.Options{
		SkipLLM:       usecaseTestsSkipLLM,
		FreeModel:     usecaseTestsFreeModel,
		ReviewRepo:    usecaseTestsReviewRepo,
		ReviewBase:    usecaseTestsReviewBase,
		ResearchTopic: usecaseTestsResearchTopic,
		HermesDir:     usecaseTestsHermesDir,
		Timeout:       usecaseTestsTimeout,
	}

	summary, err := runner.Run(ctx, opts, scenarioFilter)
	if err != nil {
		return fmt.Errorf("usecase-tests: %w", err)
	}

	// Append JSONL audit events (one per scenario + one summary line).
	if logErr := writeUsecaseLog(summary); logErr != nil {
		// Logging is best-effort; surface a warning on stderr but do not
		// fail the run for it.
		fmt.Fprintf(cmd.ErrOrStderr(), "usecase-tests: warn: log write failed: %v\n", logErr)
	}

	if !usecaseTestsQuiet {
		if format == "json" {
			if err := renderUsecaseJSON(cmd, summary); err != nil {
				return err
			}
		} else {
			renderUsecaseMarkdown(cmd, summary, opts)
		}
	}

	if summary.ExitCode() != 0 {
		// Silence cobra's "Error:" prefix; the markdown table already
		// surfaces every failed scenario.
		cmd.SilenceUsage = true
		return errors.New("usecase-tests: one or more scenarios failed")
	}
	return nil
}

func renderUsecaseJSON(cmd *cobra.Command, sum usecasetests.Summary) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(sum)
}

func renderUsecaseMarkdown(cmd *cobra.Command, sum usecasetests.Summary, opts usecasetests.Options) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "# Fleet use-case test pass — %s\n\n", sum.Generated.Format(time.RFC3339))
	fmt.Fprintf(w, "- Skip LLM: `%t`\n", opts.SkipLLM)
	fmt.Fprintf(w, "- Free-tier model: `%s`\n", opts.FreeModel)
	fmt.Fprintf(w, "- Per-scenario timeout: `%s`\n", opts.Timeout)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| ID | Scenario | Category | Status | Detail |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, r := range sum.Results {
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n",
			r.ID, r.Name, r.Category, r.Status, escapeMarkdownPipe(r.Message))
	}
	fmt.Fprintf(w, "\nTotals: **%d pass / %d warn / %d skip / %d fail** (duration: %s).\n\n",
		sum.Pass, sum.Warn, sum.Skip, sum.Fail, sum.Duration.Round(time.Millisecond))
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w)
	for _, r := range sum.Results {
		fmt.Fprintf(w, "## %s %s\n\n", r.ID, r.Name)
		fmt.Fprintf(w, "- Category: %s\n", r.Category)
		fmt.Fprintf(w, "- Status: **%s**\n", r.Status)
		fmt.Fprintf(w, "- Duration: %s\n", r.Duration.Round(time.Millisecond))
		fmt.Fprintf(w, "- Detail: %s\n\n", r.Message)
	}
}

// escapeMarkdownPipe replaces "|" with "\|" so a scenario message
// containing a pipe doesn't break the summary table column.
func escapeMarkdownPipe(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

func writeUsecaseLog(sum usecasetests.Summary) error {
	resolved, err := expandHomePath(usecaseTestsLogPath)
	if err != nil {
		return err
	}
	logger, err := openLogger(resolved)
	if err != nil {
		return err
	}
	defer logger.Close()
	node, _ := os.Hostname()
	for _, r := range sum.Results {
		result := mapStatusToLogResult(r.Status)
		_ = logger.Action(log.Event{
			Node:       node,
			Phase:      "usecase-tests",
			Action:     r.ID + ":" + r.Name,
			Result:     result,
			DurationMs: r.Duration.Milliseconds(),
			StderrTail: r.Message,
		})
	}
	// One summary line so a downstream watcher gets a single record per
	// pass — mirrors the bash original's finalize() summary event.
	overall := log.ResultOK
	if sum.Fail > 0 {
		overall = log.ResultError
	} else if sum.Warn > 0 {
		overall = log.ResultWarn
	}
	_ = logger.Action(log.Event{
		Node:       node,
		Phase:      "usecase-tests",
		Action:     "summary",
		Result:     overall,
		DurationMs: sum.Duration.Milliseconds(),
		StderrTail: fmt.Sprintf("pass=%d warn=%d skip=%d fail=%d", sum.Pass, sum.Warn, sum.Skip, sum.Fail),
	})
	return nil
}

func mapStatusToLogResult(s usecasetests.Status) log.Result {
	switch s {
	case usecasetests.StatusPass:
		return log.ResultOK
	case usecasetests.StatusWarn:
		return log.ResultWarn
	case usecasetests.StatusSkip:
		return log.ResultSkipped
	case usecasetests.StatusFail:
		return log.ResultError
	}
	return log.ResultError
}

func usecaseTestsSkill() Skill {
	return Skill{
		Name:    "usecase-tests",
		Trigger: "/usecase-tests",
		Summary: "Operator-shaped end-to-end test pass over the 10 scenarios the v2 deployment plan ships (delegation, review, research, observation, lifecycle, resilience).",
		Args: []SkillArg{
			{Name: "--scenario S1,...", Description: "Comma-separated scenario IDs. Default: all 10."},
			{Name: "--format markdown|json", Description: "Output format. Default markdown."},
			{Name: "--continue-on-fail", Description: "Run every selected scenario even after a FAIL."},
			{Name: "--skip-llm", Description: "Skip S4 and run S3 in structural-coverage-only mode."},
			{Name: "--free-model NAME", Description: "OpenRouter free-tier model for S3/S4. Default openai/gpt-oss-120b:free."},
			{Name: "--review-repo PATH", Description: "Repo for S3 git-stat. Default cwd."},
			{Name: "--review-base REF", Description: "Base ref for S3 diff. Default HEAD~5 (falls back to HEAD~1 on shallow clones)."},
			{Name: "--research-topic STR", Description: "Topic for S4 research query."},
			{Name: "--hermes-dir PATH", Description: "Override ~/.hermes (used by S5/S9/S10)."},
			{Name: "--timeout DUR", Description: "Per-scenario wall-clock cap. Default 5m."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL audit log is unchanged."},
		},
		Examples: []SkillExample{
			{Description: "Full pass", Command: "fleetctl usecase-tests"},
			{Description: "Just the delegation + observation subset", Command: "fleetctl usecase-tests --scenario S1,S2,S5,S6,S7,S8"},
			{Description: "Fast pass without LLM calls", Command: "fleetctl usecase-tests --skip-llm"},
			{Description: "JSON for piping into jq", Command: "fleetctl usecase-tests --format json"},
		},
	}
}
