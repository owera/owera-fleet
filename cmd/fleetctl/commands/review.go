// review.go implements `fleetctl review` — runs a git branch through the
// Hermes agent for a code-review report and emits a JSONL audit event.
package commands

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
)

var (
	reviewBranch    string
	reviewOut       string
	reviewJSON      bool
	reviewQuiet     bool
	reviewHermesDir string
	reviewLogPath   = "~/.hermes/logs/review.jsonl"
)

// reviewRunner is overridable in tests; production wires hermes.Client.RunZ.
// The default returns an error so callers know the hermes dependency must be
// present. Tests inject a stub that returns canned responses.
var reviewRunner = func(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("review: hermes binary required; run `hermes --version` to check")
}

var reviewCmd = &cobra.Command{
	Use:   "review",
	Short: "Code-review a git branch with the Hermes agent",
	Long: `review collects a git diff + log for the specified branch and asks the
local Hermes agent to review it for correctness, security issues, and
improvement suggestions.

Output is written as a markdown report with a standardized header.
The review event is appended to ~/.hermes/logs/review.jsonl.`,
	Example: `  fleetctl review --branch feature/my-work
  fleetctl review --out /tmp/review.md`,
	RunE: runReview,
}

func init() {
	reviewCmd.Flags().StringVar(&reviewBranch, "branch", "", "git branch to review (default: current branch)")
	reviewCmd.Flags().StringVar(&reviewOut, "out", "", "write report to FILE (default: stdout)")
	reviewCmd.Flags().BoolVar(&reviewJSON, "json", false, "emit JSON instead of markdown")
	reviewCmd.Flags().BoolVar(&reviewQuiet, "quiet", false, "suppress stdout; JSONL log still written")
	reviewCmd.Flags().StringVar(&reviewHermesDir, "hermes-dir", "", "override ~/.hermes path")
	rootCmd.AddCommand(reviewCmd)
}

func runReview(cmd *cobra.Command, _ []string) error {
	branch := reviewBranch
	if branch == "" {
		out, err := gitCurrentBranch()
		if err != nil {
			return fmt.Errorf("review: --branch not set and git branch detection failed: %w", err)
		}
		branch = out
	}

	// Gather git context.
	diffStat, _ := gitRun("diff", "main..."+branch, "--stat")
	gitLog, _ := gitRun("log", "--oneline", "-20", branch)

	prompt := fmt.Sprintf(
		"Review this git branch for correctness, security issues, and improvement suggestions.\n\nBranch: %s\n\nDiff summary:\n%s\n\nRecent commits:\n%s",
		branch, diffStat, gitLog,
	)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	start := time.Now()
	response, runErr := reviewRunner(ctx, prompt)
	dur := time.Since(start).Milliseconds()

	// Resolve and write JSONL.
	logSrc := reviewLogPath
	if reviewHermesDir != "" {
		logSrc = reviewHermesDir + "/logs/review.jsonl"
	}
	if resolvedLog, err := expandHomePath(logSrc); err == nil {
		if logger, err := openLogger(resolvedLog); err == nil {
			defer logger.Close()
			node, _ := os.Hostname()
			result := log.ResultOK
			tail := ""
			if runErr != nil {
				result = log.ResultError
				tail = runErr.Error()
			}
			_ = logger.Action(log.Event{
				Node:       node,
				Phase:      "review",
				Action:     "review:" + branch,
				Result:     result,
				DurationMs: dur,
				StderrTail: tail,
			})
		}
	}

	if runErr != nil {
		return fmt.Errorf("review: hermes run failed: %w", runErr)
	}

	report := buildReviewReport(branch, response)

	if !reviewQuiet {
		if reviewOut != "" {
			if err := os.WriteFile(reviewOut, []byte(report), 0o644); err != nil {
				return fmt.Errorf("review: write %s: %w", reviewOut, err)
			}
		} else {
			fmt.Fprint(cmd.OutOrStdout(), report)
		}
	}
	return nil
}

func buildReviewReport(branch, response string) string {
	var b strings.Builder
	b.WriteString("# Code Review: ")
	b.WriteString(branch)
	b.WriteString("\n\n")
	b.WriteString("Generated: ")
	b.WriteString(time.Now().UTC().Format(time.RFC3339))
	b.WriteString("\n\n")
	b.WriteString(response)
	if !strings.HasSuffix(response, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func gitCurrentBranch() (string, error) {
	out, err := gitRunFunc("rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

func gitRun(args ...string) (string, error) {
	return gitRunFunc(args...)
}

// gitRunFunc is overridable in tests.
var gitRunFunc = func(args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}


func reviewSkill() Skill {
	return Skill{
		Name:    "review",
		Trigger: "/review",
		Summary: "Code-review a git branch by delegating the diff to the Hermes agent.",
		Args: []SkillArg{
			{Name: "--branch BRANCH", Description: "Git branch to review (default: current branch)."},
			{Name: "--out FILE", Description: "Write markdown report to FILE (default: stdout)."},
			{Name: "--json", Description: "Emit JSON instead of markdown."},
			{Name: "--quiet", Description: "Suppress stdout; JSONL log still written."},
			{Name: "--hermes-dir PATH", Description: "Override ~/.hermes for log path."},
		},
		Examples: []SkillExample{
			{Description: "Review current branch", Command: "fleetctl review"},
			{Description: "Review a specific branch", Command: "fleetctl review --branch feature/my-work"},
			{Description: "Save report to file", Command: "fleetctl review --out /tmp/review.md"},
		},
	}
}

func init() { registerSkill("review", reviewSkill) }
