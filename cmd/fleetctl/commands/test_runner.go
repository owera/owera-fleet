// test_runner.go implements `fleetctl test` — loads scenario YAML files and
// runs them against the live fleet, reporting PASS/FAIL/GAP per scenario.
package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/log"
	"github.com/owera/owera-fleet/internal/scenarios"
)

var (
	testScenario    []string
	testTier        int
	testScenariosDir string
	testContinue    bool
	testQuiet       bool
	testJSON        bool
	testTimeout     time.Duration
	testNode        string
	testLogPath     = "~/.hermes/logs/test.jsonl"
)

// StepResult is the outcome of one scenario step.
type StepResult struct {
	Name     string
	Cmd      string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	Pass     bool
	Failure  string
}

// ScenarioResult collects all step results for one scenario.
type ScenarioResult struct {
	Scenario *scenarios.Scenario
	Steps    []StepResult
	Pass     bool
	Duration time.Duration
}

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Run fleet test scenarios from scenarios/*.yaml",
	Long: `test loads YAML scenario files and executes each step as a shell command,
checking exit code and output assertions.

Tier 1 (smoke): basic connectivity. Tier 2 (usecase): daily workflows.
Tier 3 (e2e): full end-to-end flows that may mutate state.

Results are reported as PASS/FAIL with a summary. Each run is appended
to ~/.hermes/logs/test.jsonl.`,
	Example: `  fleetctl test                            # run all scenarios
  fleetctl test --tier 1                  # only smoke scenarios
  fleetctl test --scenario smoke,health   # named subset
  fleetctl test --continue-on-fail        # don't stop at first failure`,
	RunE: runTest,
}

func init() {
	testCmd.Flags().StringSliceVar(&testScenario, "scenario", nil, "comma-separated scenario names to run")
	testCmd.Flags().IntVar(&testTier, "tier", 0, "run only scenarios at this tier (1=smoke, 2=usecase, 3=e2e)")
	testCmd.Flags().StringVar(&testScenariosDir, "scenarios-dir", "scenarios", "directory containing *.yaml scenario files")
	testCmd.Flags().BoolVar(&testContinue, "continue-on-fail", false, "keep running even if a scenario fails")
	testCmd.Flags().BoolVar(&testQuiet, "quiet", false, "suppress per-step output; JSONL log still written")
	testCmd.Flags().BoolVar(&testJSON, "json", false, "emit JSON result per scenario to stdout")
	testCmd.Flags().DurationVar(&testTimeout, "timeout", 5*time.Minute, "per-step timeout")
	testCmd.Flags().StringVar(&testNode, "node", "",
		"override the ${NODE} variable in scenarios (e.g. hermes@claw-staging.local). "+
			"Scopes any node-fanning scenario step to a single worker so staging-only "+
			"C2 runs don't drag production nodes in. When unset, scenarios use whatever "+
			"vars.NODE the YAML provides (typically a fleet-wide default).")
	rootCmd.AddCommand(testCmd)
}

// stepRunner is overridable in tests.
var stepRunner = func(ctx context.Context, cmd string) (stdout, stderr string, exit int, err error) {
	var so, se bytes.Buffer
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdout = &so
	c.Stderr = &se
	runErr := c.Run()
	exit = 0
	if c.ProcessState != nil {
		exit = c.ProcessState.ExitCode()
	}
	return so.String(), se.String(), exit, runErr
}

func runTest(cmd *cobra.Command, _ []string) error {
	resolvedLog, err := expandHomePath(testLogPath)
	if err != nil {
		return fmt.Errorf("test: expand log path: %w", err)
	}
	logger, err := openLogger(resolvedLog)
	if err != nil {
		return fmt.Errorf("test: open log: %w", err)
	}
	defer logger.Close()

	// Load scenario files.
	all, err := scenarios.LoadDir(testScenariosDir)
	if err != nil {
		return fmt.Errorf("test: load scenarios: %w", err)
	}
	if len(all) == 0 {
		return fmt.Errorf("test: no scenario files found in %s", testScenariosDir)
	}

	filtered := scenarios.Filter(all, testScenario, scenarios.Tier(testTier))
	if len(filtered) == 0 {
		return errors.New("test: no scenarios match the given filters")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	pass, fail := 0, 0
	for _, s := range filtered {
		res := runScenario(ctx, s)
		logScenarioResult(logger, res)

		if !testQuiet {
			printScenarioResult(cmd, res)
		}

		if res.Pass {
			pass++
		} else {
			fail++
			if !testContinue {
				fmt.Fprintf(cmd.ErrOrStderr(), "\ntest: stopping after first failure (use --continue-on-fail to run all)\n")
				break
			}
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\n--- SUMMARY: %d passed, %d failed ---\n", pass, fail)
	if fail > 0 {
		return fmt.Errorf("test: %d scenario(s) failed", fail)
	}
	return nil
}

func runScenario(ctx context.Context, s *scenarios.Scenario) ScenarioResult {
	res := ScenarioResult{Scenario: s}
	start := time.Now()

	// If --node was passed, inject it as a ${NODE} override that takes
	// precedence over any vars.NODE the scenario YAML provides. Lets
	// "fleetctl test --tier 3 --node hermes@claw-staging.local" scope
	// the gauntlet to staging without editing nodes.txt or YAML files.
	var overrides map[string]string
	if testNode != "" {
		overrides = map[string]string{"NODE": testNode}
	}

	for _, step := range s.Steps {
		stepCtx, cancel := context.WithTimeout(ctx, testTimeout)
		cmd := s.ExpandVars(step.Run, overrides)
		stepStart := time.Now()
		stdout, stderr, exit, _ := stepRunner(stepCtx, cmd)
		cancel()

		sr := StepResult{
			Name:     step.Name,
			Cmd:      cmd,
			ExitCode: exit,
			Stdout:   stdout,
			Stderr:   stderr,
			Duration: time.Since(stepStart),
		}

		// Check assertions.
		expected := 0
		if step.Expect.Exit != nil {
			expected = *step.Expect.Exit
		}
		if exit != expected {
			sr.Failure = fmt.Sprintf("exit=%d, want %d", exit, expected)
		} else if step.Expect.StdoutContains != "" && !strings.Contains(stdout, step.Expect.StdoutContains) {
			sr.Failure = fmt.Sprintf("stdout missing %q", step.Expect.StdoutContains)
		} else if step.Expect.StdoutAbsent != "" && strings.Contains(stdout, step.Expect.StdoutAbsent) {
			sr.Failure = fmt.Sprintf("stdout unexpectedly contains %q", step.Expect.StdoutAbsent)
		} else if step.Expect.StderrContains != "" && !strings.Contains(stderr, step.Expect.StderrContains) {
			sr.Failure = fmt.Sprintf("stderr missing %q", step.Expect.StderrContains)
		}
		sr.Pass = sr.Failure == ""

		res.Steps = append(res.Steps, sr)
		if !sr.Pass {
			res.Pass = false
			res.Duration = time.Since(start)
			return res
		}
	}

	res.Pass = true
	res.Duration = time.Since(start)
	return res
}

func logScenarioResult(logger *log.Logger, res ScenarioResult) {
	if logger == nil {
		return
	}
	node, _ := os.Hostname()
	result := log.ResultOK
	tail := ""
	if !res.Pass {
		result = log.ResultError
		for _, s := range res.Steps {
			if !s.Pass {
				tail = s.Failure
				break
			}
		}
	}
	_ = logger.Action(log.Event{
		Node:       node,
		Phase:      "test",
		Action:     fmt.Sprintf("scenario:%s:tier%d", res.Scenario.Name, int(res.Scenario.Tier)),
		Result:     result,
		DurationMs: res.Duration.Milliseconds(),
		StderrTail: tail,
	})
}

func printScenarioResult(cmd *cobra.Command, res ScenarioResult) {
	status := "PASS"
	if !res.Pass {
		status = "FAIL"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s (tier%d) %dms\n",
		status, res.Scenario.Name, int(res.Scenario.Tier), res.Duration.Milliseconds())
	if !res.Pass {
		for _, s := range res.Steps {
			if !s.Pass {
				fmt.Fprintf(cmd.OutOrStdout(), "  step %q: %s\n", s.Name, s.Failure)
				fmt.Fprintf(cmd.OutOrStdout(), "  cmd: %s\n", s.Cmd)
				if s.Stdout != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  stdout: %s\n", strings.TrimRight(s.Stdout, "\n"))
				}
				if s.Stderr != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  stderr: %s\n", strings.TrimRight(s.Stderr, "\n"))
				}
			}
		}
	}
}

func testSkill() Skill {
	return Skill{
		Name:    "test",
		Trigger: "/test",
		Summary: "Run fleet test scenarios from scenarios/*.yaml — PASS/FAIL per scenario.",
		Args: []SkillArg{
			{Name: "--scenario NAME", Description: "Comma-separated scenario names to run (default: all)."},
			{Name: "--tier N", Description: "Run only tier-N scenarios: 1=smoke, 2=usecase, 3=e2e."},
			{Name: "--scenarios-dir PATH", Description: "Directory with *.yaml scenario files (default: scenarios/)."},
			{Name: "--continue-on-fail", Description: "Keep running after the first failure."},
			{Name: "--timeout DUR", Description: "Per-step timeout (default 5m)."},
			{Name: "--quiet", Description: "Suppress per-step output; JSONL log still written."},
		},
		Examples: []SkillExample{
			{Description: "Run all smoke scenarios", Command: "fleetctl test --tier 1"},
			{Description: "Run named scenarios", Command: "fleetctl test --scenario smoke,health"},
			{Description: "Run everything, don't stop on fail", Command: "fleetctl test --continue-on-fail"},
		},
	}
}

func init() { registerSkill("test", testSkill) }
