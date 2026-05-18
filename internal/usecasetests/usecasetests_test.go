package usecasetests

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeScenario returns a Scenario whose Run produces a fixed Result.
func fakeScenario(id, name, category string, status Status, msg string) Scenario {
	return Scenario{
		ID: id, Name: name, Category: category,
		Run: func(_ context.Context, _ Options) Result {
			return Result{
				ID: id, Name: name, Category: category,
				Status: status, Message: msg,
			}
		},
	}
}

// panicScenario simulates a Run callback that does NOT set its result —
// the runner should normalize it to FAIL.
func badStatusScenario(id string) Scenario {
	return Scenario{
		ID: id, Name: "bad", Category: "test",
		Run: func(_ context.Context, _ Options) Result {
			return Result{ID: id, Name: "bad", Category: "test", Status: "weird"}
		},
	}
}

func TestRunnerAllPass(t *testing.T) {
	r := &Runner{
		Scenarios: []Scenario{
			fakeScenario("X1", "first", "test", StatusPass, "ok"),
			fakeScenario("X2", "second", "test", StatusPass, "ok"),
		},
	}
	sum, err := r.Run(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Pass != 2 || sum.Fail != 0 || sum.Warn != 0 || sum.Skip != 0 {
		t.Fatalf("unexpected tally: %+v", sum)
	}
	if sum.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0", sum.ExitCode())
	}
}

func TestRunnerStopsOnFail(t *testing.T) {
	r := &Runner{
		Scenarios: []Scenario{
			fakeScenario("X1", "first", "test", StatusPass, "ok"),
			fakeScenario("X2", "second", "test", StatusFail, "boom"),
			fakeScenario("X3", "third", "test", StatusPass, "ok"),
		},
	}
	sum, err := r.Run(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sum.Results) != 2 {
		t.Fatalf("expected to stop after FAIL; got %d results", len(sum.Results))
	}
	if sum.Fail != 1 {
		t.Errorf("Fail = %d, want 1", sum.Fail)
	}
	if sum.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1", sum.ExitCode())
	}
}

func TestRunnerContinueOnFail(t *testing.T) {
	r := &Runner{
		Scenarios: []Scenario{
			fakeScenario("X1", "first", "test", StatusPass, "ok"),
			fakeScenario("X2", "second", "test", StatusFail, "boom"),
			fakeScenario("X3", "third", "test", StatusPass, "ok"),
		},
		ContinueOnFail: true,
	}
	sum, err := r.Run(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sum.Results) != 3 {
		t.Fatalf("expected 3 results with ContinueOnFail; got %d", len(sum.Results))
	}
	if sum.Fail != 1 || sum.Pass != 2 {
		t.Errorf("tally pass=%d fail=%d, want pass=2 fail=1", sum.Pass, sum.Fail)
	}
}

func TestRunnerFilter(t *testing.T) {
	r := &Runner{
		Scenarios: []Scenario{
			fakeScenario("X1", "a", "t", StatusPass, "ok"),
			fakeScenario("X2", "b", "t", StatusPass, "ok"),
			fakeScenario("X3", "c", "t", StatusPass, "ok"),
		},
	}
	sum, err := r.Run(context.Background(), Options{}, []string{"X1", "X3"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Pass != 2 || sum.Skip != 1 {
		t.Fatalf("tally pass=%d skip=%d, want pass=2 skip=1", sum.Pass, sum.Skip)
	}
	// Confirm X2 was skipped with the expected message.
	var skipped *Result
	for i := range sum.Results {
		if sum.Results[i].ID == "X2" {
			skipped = &sum.Results[i]
			break
		}
	}
	if skipped == nil {
		t.Fatal("X2 missing from results")
	}
	if skipped.Status != StatusSkip {
		t.Errorf("X2 status = %s, want skip", skipped.Status)
	}
	if !strings.Contains(skipped.Message, "not selected") {
		t.Errorf("X2 message = %q, want substring 'not selected'", skipped.Message)
	}
}

func TestRunnerUnknownFilterIsError(t *testing.T) {
	r := &Runner{
		Scenarios: []Scenario{
			fakeScenario("X1", "a", "t", StatusPass, "ok"),
		},
	}
	_, err := r.Run(context.Background(), Options{}, []string{"X99"})
	if err == nil {
		t.Fatal("expected error for unknown scenario filter")
	}
	if !strings.Contains(err.Error(), "X99") {
		t.Errorf("error %q should mention X99", err)
	}
}

func TestRunnerBadStatusNormalizedToFail(t *testing.T) {
	r := &Runner{
		Scenarios: []Scenario{
			badStatusScenario("X1"),
		},
		ContinueOnFail: true,
	}
	sum, err := r.Run(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Fail != 1 {
		t.Errorf("Fail = %d, want 1 (bad status should normalize)", sum.Fail)
	}
}

func TestRunnerFillsDurationWhenMissing(t *testing.T) {
	r := &Runner{
		Scenarios: []Scenario{
			fakeScenario("X1", "a", "t", StatusPass, "ok"),
		},
	}
	sum, err := r.Run(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Results[0].Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", sum.Results[0].Duration)
	}
}

func TestApplyOptionDefaults(t *testing.T) {
	o := applyOptionDefaults(Options{})
	if o.FreeModel != DefaultFreeModel {
		t.Errorf("FreeModel = %q, want %q", o.FreeModel, DefaultFreeModel)
	}
	if o.ResearchTopic != DefaultResearchTopic {
		t.Errorf("ResearchTopic = %q, want %q", o.ResearchTopic, DefaultResearchTopic)
	}
	if o.ReviewBase != DefaultReviewBase {
		t.Errorf("ReviewBase = %q, want %q", o.ReviewBase, DefaultReviewBase)
	}
}

func TestDefaultScenariosShape(t *testing.T) {
	all := DefaultScenarios()
	if len(all) != 10 {
		t.Fatalf("DefaultScenarios returned %d, want 10", len(all))
	}
	wantIDs := []string{"S1", "S2", "S3", "S4", "S5", "S6", "S7", "S8", "S9", "S10"}
	for i, sc := range all {
		if sc.ID != wantIDs[i] {
			t.Errorf("scenario[%d].ID = %s, want %s", i, sc.ID, wantIDs[i])
		}
		if sc.Run == nil {
			t.Errorf("scenario[%d] %s has nil Run", i, sc.ID)
		}
	}
}

func TestRunnerHonoursTimeout(t *testing.T) {
	// Scenario that observes the context deadline; if a deadline is
	// applied, ctx.Deadline() should be set.
	r := &Runner{
		Scenarios: []Scenario{
			{
				ID: "X1", Name: "deadline-aware", Category: "t",
				Run: func(ctx context.Context, _ Options) Result {
					if _, ok := ctx.Deadline(); !ok {
						return Result{ID: "X1", Status: StatusFail, Message: "no deadline"}
					}
					return Result{ID: "X1", Status: StatusPass, Message: "deadline set"}
				},
			},
		},
	}
	sum, err := r.Run(context.Background(), Options{Timeout: 10 * time.Second}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Pass != 1 {
		t.Errorf("expected timeout-aware scenario to PASS, got %+v", sum.Results[0])
	}
}
