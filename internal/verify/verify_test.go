package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunAllAggregatesStatuses(t *testing.T) {
	checks := []Check{
		{ID: "a", Name: "alpha", Run: func(ctx context.Context) (string, error) {
			return "alpha-evidence", nil
		}},
		{ID: "b", Name: "beta", Run: func(ctx context.Context) (string, error) {
			return "", errors.New("boom")
		}},
		{ID: "c", Name: "gamma", Optional: true, Run: func(ctx context.Context) (string, error) {
			return "", errors.New("optional-fail")
		}},
	}
	r := RunAll(context.Background(), checks, 4)
	if r.Passed != 1 {
		t.Errorf("Passed = %d, want 1", r.Passed)
	}
	if r.Failed != 2 {
		t.Errorf("Failed = %d, want 2", r.Failed)
	}
	if r.OK {
		t.Error("OK = true, want false (one required fail)")
	}

	// Results must preserve input order.
	if len(r.Results) != 3 {
		t.Fatalf("want 3 results, got %d", len(r.Results))
	}
	if r.Results[0].ID != "a" || r.Results[1].ID != "b" || r.Results[2].ID != "c" {
		t.Errorf("results out of order: %+v", r.Results)
	}

	// Evidence captured from a passing check.
	if r.Results[0].Evidence != "alpha-evidence" {
		t.Errorf("evidence = %q, want %q", r.Results[0].Evidence, "alpha-evidence")
	}
	// Optional fail must not flip OK if it's the only fail.
	onlyOptional := []Check{checks[2]}
	r2 := RunAll(context.Background(), onlyOptional, 4)
	if !r2.OK {
		t.Error("OK = false with only optional-failure; want true")
	}
}

func TestRunAllConcurrency(t *testing.T) {
	const N = 12
	checks := make([]Check, N)
	for i := 0; i < N; i++ {
		checks[i] = Check{ID: idN(i), Name: "c", Run: func(ctx context.Context) (string, error) {
			time.Sleep(50 * time.Millisecond)
			return "", nil
		}}
	}
	start := time.Now()
	r := RunAll(context.Background(), checks, 4)
	elapsed := time.Since(start)

	if r.Passed != N {
		t.Errorf("Passed = %d, want %d", r.Passed, N)
	}
	// 12 checks * 50ms / 4 parallel = ~150ms; allow generous headroom.
	if elapsed > 2*time.Second {
		t.Errorf("RunAll(maxParallel=4) too slow: %v", elapsed)
	}
}

func TestFilterByIDs(t *testing.T) {
	checks := []Check{
		{ID: "a"},
		{ID: "b"},
		{ID: "c"},
	}
	got := FilterByIDs(checks, []string{"b"})
	if len(got) != 1 || got[0].ID != "b" {
		t.Errorf("FilterByIDs single: got %+v", got)
	}
	got = FilterByIDs(checks, []string{"a", "c"})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("FilterByIDs multi: got %+v", got)
	}
	got = FilterByIDs(checks, nil)
	if len(got) != 3 {
		t.Errorf("FilterByIDs empty: got %d, want 3", len(got))
	}
}

func TestFindRepoRoot(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FindRepoRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("FindRepoRoot = %q, want %q", got, dir)
	}

	// No go.mod above => error.
	empty := t.TempDir()
	if _, err := FindRepoRoot(empty); err == nil {
		t.Error("expected error for no-go.mod-above tree")
	}
}

func TestSelfChecksAllPass(t *testing.T) {
	// Each individual self-test must work in isolation. We assemble them
	// out of DefaultChecks so we cover the exact wiring shipped.
	checks := DefaultChecks("dummy") // shell-out checks will fail but we
	// filter them out below.
	var selfOnly []Check
	wanted := map[string]bool{
		"ledger_self":     true,
		"markers_self":    true,
		"pairing_self":    true,
		"budget_self":     true,
		"configsync_self": true,
		"metrics_self":    true,
	}
	for _, c := range checks {
		if wanted[c.ID] {
			selfOnly = append(selfOnly, c)
		}
	}
	if len(selfOnly) != len(wanted) {
		t.Fatalf("self-check wiring missing: got %d, want %d", len(selfOnly), len(wanted))
	}
	r := RunAll(context.Background(), selfOnly, 4)
	if !r.OK {
		for _, res := range r.Results {
			if res.Status == StatusFail {
				t.Errorf("self-check %s failed: %s\nevidence: %s", res.ID, res.ErrMsg, res.Evidence)
			}
		}
	}
}

func TestDefaultChecksRepoOverrideValidatesGoMod(t *testing.T) {
	// Passing a dir without go.mod surfaces as a clean per-check failure,
	// not a panic. Shell-out checks are flagged Optional+failing in this
	// configuration so Report.OK still reflects the passing self-checks —
	// this is the documented "fleetctl verify on a deployed worker
	// without a source tree" scenario.
	empty := t.TempDir()
	checks := DefaultChecks(empty)
	r := RunAll(context.Background(), checks, 4)

	for _, res := range r.Results {
		if strings.HasSuffix(res.ID, "_self") {
			if res.Status != StatusPass {
				t.Errorf("self-check %s failed unexpectedly: %s", res.ID, res.ErrMsg)
			}
			continue
		}
		// Shell-out checks: must be Optional and must have failed,
		// since the dummy repo dir has no go.mod.
		if !res.Optional {
			t.Errorf("shell-out check %s should be Optional when repo root has no go.mod (would block OK gate on deployed workers)", res.ID)
		}
		if res.Status != StatusFail {
			t.Errorf("shell-out check %s: expected fail, got %s", res.ID, res.Status)
		}
	}
	if !r.OK {
		t.Errorf("Report.OK should be true (only Optional shell-out checks failed): Passed=%d Failed=%d Skipped=%d",
			r.Passed, r.Failed, r.Skipped)
	}
}

func idN(i int) string {
	return "id-" + string(rune('a'+i%26))
}
