package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/usecasetests"
)

func resetUsecaseTestsFlags() {
	usecaseTestsScenario = nil
	usecaseTestsFormat = "markdown"
	usecaseTestsAll = true
	usecaseTestsContinueOnFail = false
	usecaseTestsSkipLLM = false
	usecaseTestsFreeModel = usecasetests.DefaultFreeModel
	usecaseTestsReviewRepo = ""
	usecaseTestsReviewBase = usecasetests.DefaultReviewBase
	usecaseTestsResearchTopic = usecasetests.DefaultResearchTopic
	usecaseTestsHermesDir = ""
	usecaseTestsTimeout = 5 * time.Minute
	usecaseTestsQuiet = false
}

// fakeScenario returns a scenario with a fixed result, mirroring the
// helper in the internal package's tests but defined locally so this
// test file doesn't import usecasetests internals.
func fakeUsecaseScenario(id, name, category string, status usecasetests.Status, msg string) usecasetests.Scenario {
	return usecasetests.Scenario{
		ID: id, Name: name, Category: category,
		Run: func(_ context.Context, _ usecasetests.Options) usecasetests.Result {
			return usecasetests.Result{
				ID: id, Name: name, Category: category,
				Status: status, Message: msg,
			}
		},
	}
}

// installFakeRunner swaps newUsecaseTestsRunner so tests can drive the
// cobra command with deterministic scenarios.
func installFakeRunner(t *testing.T, scenarios []usecasetests.Scenario) func() {
	t.Helper()
	prev := newUsecaseTestsRunner
	newUsecaseTestsRunner = func() *usecasetests.Runner {
		return &usecasetests.Runner{Scenarios: scenarios}
	}
	return func() { newUsecaseTestsRunner = prev }
}

// withTempLog redirects the JSONL log to a temp file so tests don't
// touch ~/.hermes.
func withTempLog(t *testing.T) func() {
	t.Helper()
	tmp := t.TempDir()
	prev := usecaseTestsLogPath
	usecaseTestsLogPath = filepath.Join(tmp, "usecase-tests.jsonl")
	return func() { usecaseTestsLogPath = prev }
}

func TestUsecaseTestsMarkdownAllPass(t *testing.T) {
	resetUsecaseTestsFlags()
	defer resetUsecaseTestsFlags()
	defer installFakeRunner(t, []usecasetests.Scenario{
		fakeUsecaseScenario("S1", "first", "delegation", usecasetests.StatusPass, "ok"),
		fakeUsecaseScenario("S2", "second", "delegation", usecasetests.StatusPass, "ok"),
	})()
	defer withTempLog(t)()

	var buf bytes.Buffer
	usecaseTestsCmd.SetOut(&buf)
	usecaseTestsCmd.SetErr(&buf)
	defer usecaseTestsCmd.SetOut(nil)
	defer usecaseTestsCmd.SetErr(nil)

	if err := runUsecaseTests(usecaseTestsCmd, nil); err != nil {
		t.Fatalf("runUsecaseTests: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Fleet use-case test pass") {
		t.Errorf("output missing header: %s", out)
	}
	if !strings.Contains(out, "| S1 | first | delegation | pass | ok |") {
		t.Errorf("output missing S1 row: %s", out)
	}
	if !strings.Contains(out, "2 pass / 0 warn / 0 skip / 0 fail") {
		t.Errorf("output missing tally: %s", out)
	}
}

func TestUsecaseTestsJSONFormat(t *testing.T) {
	resetUsecaseTestsFlags()
	defer resetUsecaseTestsFlags()
	usecaseTestsFormat = "json"
	defer installFakeRunner(t, []usecasetests.Scenario{
		fakeUsecaseScenario("S1", "first", "delegation", usecasetests.StatusPass, "ok"),
	})()
	defer withTempLog(t)()

	var buf bytes.Buffer
	usecaseTestsCmd.SetOut(&buf)
	defer usecaseTestsCmd.SetOut(nil)

	if err := runUsecaseTests(usecaseTestsCmd, nil); err != nil {
		t.Fatalf("runUsecaseTests: %v", err)
	}
	var sum usecasetests.Summary
	if err := json.Unmarshal(buf.Bytes(), &sum); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, buf.String())
	}
	if sum.Pass != 1 {
		t.Errorf("summary.Pass = %d, want 1", sum.Pass)
	}
}

func TestUsecaseTestsFailReturnsError(t *testing.T) {
	resetUsecaseTestsFlags()
	defer resetUsecaseTestsFlags()
	defer installFakeRunner(t, []usecasetests.Scenario{
		fakeUsecaseScenario("S1", "first", "delegation", usecasetests.StatusFail, "boom"),
	})()
	defer withTempLog(t)()

	var buf bytes.Buffer
	usecaseTestsCmd.SetOut(&buf)
	usecaseTestsCmd.SetErr(&buf)
	defer usecaseTestsCmd.SetOut(nil)
	defer usecaseTestsCmd.SetErr(nil)

	err := runUsecaseTests(usecaseTestsCmd, nil)
	if err == nil {
		t.Fatal("expected error when scenarios FAIL")
	}
}

func TestUsecaseTestsInvalidFormat(t *testing.T) {
	resetUsecaseTestsFlags()
	defer resetUsecaseTestsFlags()
	usecaseTestsFormat = "yaml"
	if err := runUsecaseTests(usecaseTestsCmd, nil); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestUsecaseTestsScenarioFilter(t *testing.T) {
	resetUsecaseTestsFlags()
	defer resetUsecaseTestsFlags()
	usecaseTestsScenario = []string{"S1"}
	defer installFakeRunner(t, []usecasetests.Scenario{
		fakeUsecaseScenario("S1", "first", "delegation", usecasetests.StatusPass, "ok"),
		fakeUsecaseScenario("S2", "second", "delegation", usecasetests.StatusPass, "ok"),
	})()
	defer withTempLog(t)()

	var buf bytes.Buffer
	usecaseTestsCmd.SetOut(&buf)
	defer usecaseTestsCmd.SetOut(nil)
	if err := runUsecaseTests(usecaseTestsCmd, nil); err != nil {
		t.Fatalf("runUsecaseTests: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 pass / 0 warn / 1 skip") {
		t.Errorf("tally missing: %s", out)
	}
}
