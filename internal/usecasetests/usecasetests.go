// Package usecasetests is the Go-native successor to
// hermes-setup/scripts/fleet-usecase-tests.sh: an operator-shaped
// end-to-end test pass for the use cases the v2 deployment plan already
// ships.
//
// Each scenario simulates one real operator workflow — delegation, review,
// research, observability, lifecycle, resilience — and reports
// PASS / WARN / SKIP / FAIL plus a one-line message. The bash original's
// 10 scenarios (S1..S10) all carry over with the same IDs; the migration
// preserves the contract that S3/S4 may WARN when free-tier LLM responses
// degrade, that S2 WARNs on a one-node fleet, and that S10 WARNs (never
// FAILs) when an unattended-job marker is stale.
//
// # Why Go
//
// The bash version glues together six other bash scripts via os/exec and
// pipes JSON through sed/grep. Most of those subordinate scripts now have
// fleetctl Go equivalents (`delegate`, `review`, `research`, `health`,
// `update`), which means the Go runner can call internal/* packages
// directly and skip the os/exec round-trip. The handful of scenarios whose
// underlying script has NOT been ported yet (S6 drift-diff, S7 process
// reachability, S8 dotfile drift) degrade to a SKIP with an explicit "not yet
// ported" message rather than shelling out to the bash original; that
// keeps this package single-language and avoids spawning hidden bash
// subprocesses from the fleetctl binary.
//
// # Concurrency
//
// Run is single-goroutine — scenarios run sequentially, which matches the
// bash version's behaviour (it greps the same nodes.txt across scenarios
// and writes to the same JSONL log). If a future scenario needs
// parallelism, run them in their own goroutine and join in the runner.
package usecasetests

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Status is the per-scenario outcome.
//
// PASS  — workflow completed and the spot-check passed.
// WARN  — workflow completed but a softer expectation was not met (e.g.
//
//	free-tier LLM returned an empty Summary, only one worker so fan-out
//	is trivially satisfied).
//
// SKIP  — scenario explicitly suppressed (--scenario filter excluded it,
//
//	--skip-llm masked an LLM-bearing scenario, or no Go primitive exists
//	yet for the underlying probe).
//
// FAIL  — workflow errored or the spot-check failed in a way that should
//
//	block the operator's confidence in the fleet.
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusSkip Status = "skip"
	StatusFail Status = "fail"
)

// String returns the lowercase canonical name.
func (s Status) String() string { return string(s) }

// Result is the outcome of one scenario invocation.
type Result struct {
	// ID is the scenario short identifier ("S1" .. "S10").
	ID string `json:"id"`
	// Name is the human-readable title.
	Name string `json:"name"`
	// Category groups scenarios for the summary table (delegation,
	// review, research, observation, lifecycle, resilience).
	Category string `json:"category"`
	// Status is the PASS/WARN/SKIP/FAIL outcome.
	Status Status `json:"status"`
	// Message is the one-line reason / detail for the status.
	Message string `json:"message"`
	// Duration is wall-clock time spent running the scenario body.
	Duration time.Duration `json:"duration_ns"`
}

// Scenario is one named test in the harness.
//
// Run executes the workflow and returns a Result with the same ID and
// Name as the scenario itself; the Runner does not mutate the Result
// after the scenario returns (other than filling Duration if a scenario
// neglects to). Implementations should never panic — return
// FAIL + Message for any unexpected condition.
type Scenario struct {
	ID       string
	Name     string
	Category string
	Run      func(ctx context.Context, opts Options) Result
}

// Options carries the harness-wide knobs passed to every Scenario.Run.
//
// Tests construct Options directly; the cobra wrapper translates command
// flags into the struct.
type Options struct {
	// SkipLLM masks the LLM-bearing scenarios (S3, S4): S3 runs in
	// structural-coverage-only mode (no Hermes call); S4 returns SKIP.
	SkipLLM bool

	// FreeModel is the OpenRouter free-tier model passed to S3/S4.
	// Default chosen by the runner if empty.
	FreeModel string

	// ReviewRepo is the path to the git repo S3 should diff against.
	// Default chosen by the runner if empty.
	ReviewRepo string

	// ReviewBase is the base ref S3 should diff against (e.g. HEAD~5).
	// Default chosen by the runner if empty.
	ReviewBase string

	// ResearchTopic is the topic string S4 will research.
	ResearchTopic string

	// HermesDir overrides ~/.hermes (used by S5, S7, S9, S10 for
	// log/marker inspection). Default $HOME/.hermes when empty.
	HermesDir string

	// Timeout caps each scenario's run. Zero means no cap.
	Timeout time.Duration
}

// DefaultFreeModel matches the bash original's default. Override with
// --free-model if upstream availability shifts.
const DefaultFreeModel = "openai/gpt-oss-120b:free"

// DefaultResearchTopic matches the bash original.
const DefaultResearchTopic = "Hermes Agent v0.13 release notes summary"

// DefaultReviewBase matches the bash original's review base.
const DefaultReviewBase = "HEAD~5"

// Summary is the aggregate of one Run.
type Summary struct {
	// Results in registration order (same as the bash version's S1..S10
	// output order).
	Results []Result `json:"results"`
	// Pass/Warn/Skip/Fail are the per-status tallies.
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Skip int `json:"skip"`
	Fail int `json:"fail"`
	// Generated is the runner's wall-clock time at the end of the pass.
	Generated time.Time `json:"generated"`
	// Duration is the cumulative wall-clock time for all scenarios.
	Duration time.Duration `json:"duration_ns"`
}

// ExitCode returns 0 if no scenario FAILed, 1 otherwise. WARN and SKIP
// are not failure conditions — they match the bash version's exit
// contract.
func (s Summary) ExitCode() int {
	if s.Fail > 0 {
		return 1
	}
	return 0
}

// Runner orchestrates Scenario execution.
type Runner struct {
	// Scenarios is the ordered list to dispatch (filtered by IDs in
	// Options-via-Run). Tests construct a Runner directly with mock
	// scenarios; production wires DefaultScenarios.
	Scenarios []Scenario
	// ContinueOnFail, when true, runs every selected scenario even
	// after one FAILs. Default behaviour stops at the first failure
	// (mirrors the bash original).
	ContinueOnFail bool
}

// Run dispatches scenarios. filter is an optional set of scenario IDs
// to include; an empty filter runs everything. Filtering by an unknown
// ID returns an error so typos surface before the harness silently
// no-ops.
func (r *Runner) Run(ctx context.Context, opts Options, filter []string) (Summary, error) {
	allowed, err := buildFilter(r.Scenarios, filter)
	if err != nil {
		return Summary{}, err
	}
	opts = applyOptionDefaults(opts)
	var sum Summary
	for _, sc := range r.Scenarios {
		if !allowed[sc.ID] {
			res := Result{
				ID: sc.ID, Name: sc.Name, Category: sc.Category,
				Status: StatusSkip, Message: "not selected",
			}
			sum.Results = append(sum.Results, res)
			sum.Skip++
			continue
		}
		start := time.Now()
		scCtx := ctx
		var cancel context.CancelFunc
		if opts.Timeout > 0 {
			scCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		}
		res := sc.Run(scCtx, opts)
		if cancel != nil {
			cancel()
		}
		if res.Duration == 0 {
			res.Duration = time.Since(start)
		}
		// Force the Result identity columns to match the registration —
		// a scenario that returns a different ID would corrupt the
		// summary table.
		res.ID = sc.ID
		res.Name = sc.Name
		res.Category = sc.Category
		sum.Results = append(sum.Results, res)
		sum.Duration += res.Duration
		switch res.Status {
		case StatusPass:
			sum.Pass++
		case StatusWarn:
			sum.Warn++
		case StatusSkip:
			sum.Skip++
		case StatusFail:
			sum.Fail++
		default:
			sum.Fail++
			res.Status = StatusFail
			res.Message = fmt.Sprintf("invalid status %q from scenario", res.Status)
		}
		if res.Status == StatusFail && !r.ContinueOnFail {
			break
		}
	}
	sum.Generated = time.Now().UTC()
	return sum, nil
}

// buildFilter returns the set of scenario IDs that should run.
//
// An empty filter means "all". Unknown IDs in the filter cause an
// explicit error so the operator's typo is caught up front rather than
// silently producing an empty pass.
func buildFilter(scenarios []Scenario, filter []string) (map[string]bool, error) {
	known := make(map[string]bool, len(scenarios))
	for _, sc := range scenarios {
		known[sc.ID] = true
	}
	if len(filter) == 0 {
		out := make(map[string]bool, len(known))
		for id := range known {
			out[id] = true
		}
		return out, nil
	}
	out := make(map[string]bool, len(filter))
	for _, raw := range filter {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if !known[id] {
			return nil, fmt.Errorf("unknown scenario %q (known: %s)", id, knownIDs(scenarios))
		}
		out[id] = true
	}
	return out, nil
}

func knownIDs(scenarios []Scenario) string {
	parts := make([]string, 0, len(scenarios))
	for _, sc := range scenarios {
		parts = append(parts, sc.ID)
	}
	return strings.Join(parts, ", ")
}

func applyOptionDefaults(o Options) Options {
	if o.FreeModel == "" {
		o.FreeModel = DefaultFreeModel
	}
	if o.ResearchTopic == "" {
		o.ResearchTopic = DefaultResearchTopic
	}
	if o.ReviewBase == "" {
		o.ReviewBase = DefaultReviewBase
	}
	return o
}
