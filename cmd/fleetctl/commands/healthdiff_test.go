package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/owera/owera-fleet/internal/healthdiff"
)

// healthDiffRun is a thin testing harness that mirrors what cobra
// would do — initialise a captured command with stdout/stderr buffers,
// reset the package-level flag state, and call runHealthDiff directly.
// We don't go through rootCmd.Execute because the rest of the
// commands/ test suite stays out of cobra's parser too (see backup_test
// and logrotate_test) and exercising the parser would couple us to
// unrelated subcommands' init() side effects.
type healthDiffRun struct {
	out      *bytes.Buffer
	errb     *bytes.Buffer
	exitCode int
	err      error
}

func runHealthDiffFn(t *testing.T, args []string, opts ...func()) healthDiffRun {
	t.Helper()
	// Reset flag globals to defaults; tests opt in to non-defaults
	// via the opts closures.
	origFrom, origTo := healthDiffFromPath, healthDiffToPath
	origFmt, origLog := healthDiffFormat, healthDiffSuppressLog
	healthDiffFromPath = ""
	healthDiffToPath = ""
	healthDiffFormat = "markdown"
	healthDiffSuppressLog = true // tests never want to touch ~/.hermes
	t.Cleanup(func() {
		healthDiffFromPath = origFrom
		healthDiffToPath = origTo
		healthDiffFormat = origFmt
		healthDiffSuppressLog = origLog
	})
	for _, o := range opts {
		o()
	}

	captured := -1
	origExit := healthDiffExit
	healthDiffExit = func(code int) { captured = code }
	t.Cleanup(func() { healthDiffExit = origExit })

	var out, errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetContext(context.Background())
	err := runHealthDiff(cmd, args)
	return healthDiffRun{out: &out, errb: &errb, exitCode: captured, err: err}
}

// testdataPath returns the absolute path under
// internal/healthdiff/testdata/. Tests in cmd/fleetctl/commands cannot
// share testdata directly (Go test isolation rules), so we read the
// fixtures from the internal package's testdata directory.
func testdataPath(name string) string {
	return filepath.Join("..", "..", "..", "internal", "healthdiff", "testdata", name)
}

func TestHealthDiff_ExplicitPaths_Markdown(t *testing.T) {
	from := testdataPath("a.md")
	to := testdataPath("b.md")

	r := runHealthDiffFn(t, []string{from, to})
	if r.err != nil {
		t.Fatalf("runHealthDiff: %v", r.err)
	}
	if !strings.Contains(r.out.String(), "# Fleet health snapshot diff") {
		t.Errorf("markdown header missing:\n%s", r.out.String())
	}
	if !strings.Contains(r.out.String(), "**NEW HOST**") {
		t.Errorf("expected NEW HOST marker:\n%s", r.out.String())
	}
	if r.exitCode != 2 {
		t.Errorf("exit code: got %d want 2 (warn-or-crit)", r.exitCode)
	}
}

func TestHealthDiff_ExplicitPaths_JSON(t *testing.T) {
	from := testdataPath("a.md")
	to := testdataPath("b.md")

	r := runHealthDiffFn(t, []string{from, to}, func() {
		healthDiffFormat = "json"
	})
	if r.err != nil {
		t.Fatalf("runHealthDiff: %v", r.err)
	}
	if r.exitCode != 2 {
		t.Errorf("exit code: got %d want 2", r.exitCode)
	}

	var rep healthdiff.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.out.String())), &rep); err != nil {
		t.Fatalf("Unmarshal: %v\nbody=%s", err, r.out.String())
	}
	if rep.TotalChanges == 0 {
		t.Errorf("expected non-zero changes in JSON output")
	}
	if rep.GeneratedAt == "" {
		t.Errorf("generated_at must be populated")
	}
	if rep.FromName != "a.md" || rep.ToName != "b.md" {
		t.Errorf("from/to basenames: got %q / %q", rep.FromName, rep.ToName)
	}
	for _, c := range rep.Changes {
		switch c.Severity {
		case healthdiff.SeverityInfo, healthdiff.SeverityWarn, healthdiff.SeverityCrit:
		default:
			t.Errorf("invalid severity on change %+v", c)
		}
	}
}

func TestHealthDiff_Identical_ExitZero(t *testing.T) {
	a := testdataPath("a.md")
	r := runHealthDiffFn(t, []string{a, a})
	if r.err != nil {
		t.Fatalf("runHealthDiff: %v", r.err)
	}
	// healthDiffExit isn't called when there are no changes; the
	// recorded exit code stays at the sentinel -1, which corresponds
	// to a natural 0 exit from main().
	if r.exitCode != -1 {
		t.Errorf("exit code: got %d want -1 (sentinel; no exit call)", r.exitCode)
	}
	if !strings.Contains(r.out.String(), "_No changes between the two snapshots._") {
		t.Errorf("expected no-change text in output:\n%s", r.out.String())
	}
}

func TestHealthDiff_BadFormatRejected(t *testing.T) {
	a := testdataPath("a.md")
	b := testdataPath("b.md")
	r := runHealthDiffFn(t, []string{a, b}, func() {
		healthDiffFormat = "yaml"
	})
	if r.err == nil {
		t.Fatalf("expected error on unknown format")
	}
	if !strings.Contains(r.err.Error(), "unknown --format") {
		t.Errorf("error msg: %v", r.err)
	}
}

func TestHealthDiff_MissingFile(t *testing.T) {
	r := runHealthDiffFn(t, []string{
		"/tmp/this/path/does/not/exist.md",
		testdataPath("b.md"),
	})
	if r.err == nil {
		t.Fatalf("expected error on missing --from file")
	}
}

func TestHealthDiff_DefaultsFromReportsDir(t *testing.T) {
	tmp := t.TempDir()
	from := filepath.Join(tmp, "fleet-snapshot-20260515T215500Z.md")
	to := filepath.Join(tmp, "fleet-snapshot-20260515T220009Z.md")

	aBody, err := os.ReadFile(testdataPath("a.md"))
	if err != nil {
		t.Fatal(err)
	}
	bBody, err := os.ReadFile(testdataPath("b.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(from, aBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, bBody, 0o644); err != nil {
		t.Fatal(err)
	}

	origDir := healthDiffReportsDir
	healthDiffReportsDir = tmp
	t.Cleanup(func() { healthDiffReportsDir = origDir })

	r := runHealthDiffFn(t, nil)
	if r.err != nil {
		t.Fatalf("runHealthDiff: %v", r.err)
	}
	if r.exitCode != 2 {
		t.Errorf("exit: got %d want 2", r.exitCode)
	}
	if !strings.Contains(r.out.String(), "fleet-snapshot-20260515T215500Z.md") {
		t.Errorf("from filename not in output:\n%s", r.out.String())
	}
	if !strings.Contains(r.out.String(), "fleet-snapshot-20260515T220009Z.md") {
		t.Errorf("to filename not in output:\n%s", r.out.String())
	}
}

func TestHealthDiff_DefaultsFromReportsDir_TooFew(t *testing.T) {
	tmp := t.TempDir()
	only := filepath.Join(tmp, "fleet-snapshot-20260515T215500Z.md")
	if err := os.WriteFile(only, []byte("# empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origDir := healthDiffReportsDir
	healthDiffReportsDir = tmp
	t.Cleanup(func() { healthDiffReportsDir = origDir })

	r := runHealthDiffFn(t, nil)
	if r.err == nil {
		t.Fatalf("expected error when fewer than 2 snapshots present")
	}
	if !strings.Contains(r.err.Error(), "at least 2 snapshots") {
		t.Errorf("error wording: %v", r.err)
	}
}

func TestHealthDiff_FromOnly_Errors(t *testing.T) {
	a := testdataPath("a.md")
	r := runHealthDiffFn(t, nil, func() { healthDiffFromPath = a })
	if r.err == nil {
		t.Fatalf("expected error when only --from is given")
	}
	if !strings.Contains(r.err.Error(), "specified together") {
		t.Errorf("error wording: %v", r.err)
	}
}

func TestHealthDiff_PositionalCountMismatch(t *testing.T) {
	r := runHealthDiffFn(t, []string{"only-one.md"})
	if r.err == nil {
		t.Fatalf("expected error on single positional arg")
	}
	if !strings.Contains(r.err.Error(), "two positional args") {
		t.Errorf("error wording: %v", r.err)
	}
}

func TestHealthDiff_ResultForSeverity(t *testing.T) {
	cases := map[healthdiff.Severity]string{
		healthdiff.SeverityCrit: "error",
		healthdiff.SeverityWarn: "warn",
		healthdiff.SeverityInfo: "ok",
		"":                      "no-change",
	}
	for sev, want := range cases {
		got := string(resultForSeverity(sev))
		if got != want {
			t.Errorf("resultForSeverity(%q): got %q want %q", sev, got, want)
		}
	}
}
