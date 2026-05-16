// Package verify implements the WS-12 Phase-1 verification gate: a small
// concurrent runner that turns the project's internal "is the build
// shippable?" checks into a structured report.
//
// The runner is intentionally agnostic about *what* a check does. Each
// [Check] is a named function returning an error; the runner times it,
// classifies the outcome (pass / fail / skip), captures a short freeform
// evidence string, and aggregates the results into a [Report].
//
// [DefaultChecks] is the canonical set wired up by `fleetctl verify`. It
// covers the four shell-out checks (go build, go vet, go test, gen-skills
// drift) plus six in-process self-tests that exercise the operator-plane
// libraries (ledger, markers, pairing, budget, configsync, metrics).
//
// Checks marked Optional do not contribute to the OK gate. The intended use
// is environments where the optional check legitimately cannot run (e.g.
// no `go` binary on PATH inside a stripped-down release container). A
// failing optional check still appears in the report so the operator sees
// it; it just doesn't flip Report.OK to false.
package verify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/owera/owera-fleet/internal/budget"
	"github.com/owera/owera-fleet/internal/configsync"
	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/markers"
	"github.com/owera/owera-fleet/internal/metrics"
	"github.com/owera/owera-fleet/internal/pairing"
)

// Status is the outcome category of a single check.
type Status string

const (
	// StatusPass: the check ran and returned nil.
	StatusPass Status = "pass"
	// StatusFail: the check ran and returned a non-nil error.
	StatusFail Status = "fail"
	// StatusSkip: the check was filtered out (--check selector) or
	// declined to run (e.g. shell-out target missing) without producing
	// a verdict.
	StatusSkip Status = "skip"
)

// Check is a single verifiable invariant.
//
// Run returns nil for pass, non-nil for fail. The runner captures
// Run's returned error message into Result.ErrMsg. Evidence is filled by
// the check itself via *Result on the way out — see runOne.
type Check struct {
	ID       string
	Name     string
	Run      func(ctx context.Context) (evidence string, err error)
	Optional bool // failures of optional checks do not gate Report.OK
}

// Result is the outcome of running one Check.
type Result struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Status   Status        `json:"status"`
	Duration time.Duration `json:"duration_ns"`
	ErrMsg   string        `json:"err_msg,omitempty"`
	Evidence string        `json:"evidence,omitempty"`
	Optional bool          `json:"optional,omitempty"`
}

// Report aggregates every Result from a single RunAll invocation.
type Report struct {
	Results   []Result  `json:"results"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Passed    int       `json:"passed"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
	// OK is true iff every required (non-Optional) check passed.
	OK bool `json:"ok"`
}

// Elapsed returns the wall-clock time the report covers.
func (r Report) Elapsed() time.Duration { return r.EndedAt.Sub(r.StartedAt) }

// RunAll executes every check concurrently (bounded by maxParallel) and
// returns the aggregated Report. The Results slice is returned sorted by
// the input check order, not completion order, so the report is
// deterministic for snapshot comparisons.
//
// If maxParallel <= 0, a sensible default of 4 is used.
func RunAll(ctx context.Context, checks []Check, maxParallel int) Report {
	if maxParallel <= 0 {
		maxParallel = 4
	}
	start := time.Now()
	results := make([]Result, len(checks))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i := range checks {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = runOne(ctx, checks[i])
		}()
	}
	wg.Wait()
	end := time.Now()

	r := Report{Results: results, StartedAt: start, EndedAt: end, OK: true}
	for _, res := range results {
		switch res.Status {
		case StatusPass:
			r.Passed++
		case StatusFail:
			r.Failed++
			if !res.Optional {
				r.OK = false
			}
		case StatusSkip:
			r.Skipped++
		}
	}
	return r
}

func runOne(ctx context.Context, c Check) Result {
	res := Result{ID: c.ID, Name: c.Name, Optional: c.Optional}
	start := time.Now()
	evidence, err := c.Run(ctx)
	res.Duration = time.Since(start)
	res.Evidence = evidence
	if err != nil {
		res.Status = StatusFail
		res.ErrMsg = err.Error()
		return res
	}
	res.Status = StatusPass
	return res
}

// FilterByIDs returns the subset of checks whose ID matches one of ids.
// Used by `fleetctl verify --check ID` to run a single check. An empty
// ids slice returns the input unchanged.
func FilterByIDs(checks []Check, ids []string) []Check {
	if len(ids) == 0 {
		return checks
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]Check, 0, len(ids))
	for _, c := range checks {
		if want[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// FindRepoRoot walks up from start looking for a go.mod file. Used by
// shell-out checks so they invoke go and fleetctl with cwd=repo root
// regardless of where the verify command was invoked from.
//
// Returns an error if no go.mod is found before hitting the filesystem
// root.
func FindRepoRoot(start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("verify: getwd: %w", err)
		}
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("verify: no go.mod found above %s", start)
		}
		dir = parent
	}
}

// DefaultChecks returns the canonical Phase-1 gate. repoRoot is the
// directory where shell-out checks (go build/vet/test, gen-skills --check)
// run; for the self-test checks it is unused.
//
// If repoRoot is empty, FindRepoRoot is invoked from the current working
// directory. A failure to locate the repo flags the shell-out checks as
// optional-and-failing, so the rest of the gate still runs cleanly in
// environments without a go.mod (e.g. release binary on a worker).
func DefaultChecks(repoRoot string) []Check {
	resolved, repoErr := resolveRepoRoot(repoRoot)

	return []Check{
		shellOutCheck("go_build", "Build succeeds",
			resolved, repoErr,
			[]string{"go", "build", "./..."},
			"all packages compile"),

		shellOutCheck("go_vet", "Vet clean",
			resolved, repoErr,
			[]string{"go", "vet", "./..."},
			"go vet ./..."),

		shellOutCheck("go_test", "Tests pass",
			resolved, repoErr,
			[]string{"go", "test", "-race", "./..."},
			"go test -race ./..."),

		shellOutCheck("skills_drift", "Skill manifests in sync",
			resolved, repoErr,
			[]string{"go", "run", "./cmd/fleetctl", "gen-skills", "--check"},
			"gen-skills --check"),

		{ID: "ledger_self", Name: "Ledger round-trips", Run: checkLedgerSelf},
		{ID: "markers_self", Name: "Markers round-trip", Run: checkMarkersSelf},
		{ID: "pairing_self", Name: "Pairing round-trip", Run: checkPairingSelf},
		{ID: "budget_self", Name: "Budget enforces caps", Run: checkBudgetSelf},
		{ID: "configsync_self", Name: "Configsync verifies", Run: checkConfigsyncSelf},
		{ID: "metrics_self", Name: "Metrics render", Run: checkMetricsSelf},
	}
}

// resolveRepoRoot is a wrapper around FindRepoRoot that lets shell-out
// checks render the surfaced error inside their evidence rather than
// fatally on construction.
func resolveRepoRoot(override string) (string, error) {
	if override != "" {
		// Trust the caller — still verify go.mod is there so we surface
		// the problem at construction.
		if _, err := os.Stat(filepath.Join(override, "go.mod")); err != nil {
			return "", fmt.Errorf("verify: --repo %s has no go.mod: %w", override, err)
		}
		return override, nil
	}
	return FindRepoRoot("")
}

// shellOutCheck builds a Check that execs an external command with cwd set
// to the resolved repo root. If repoErr is non-nil (no go.mod found and no
// override supplied) the check is marked Optional+failing so the rest of
// the gate still passes — this is the documented "verify on a deployed
// worker without a source tree" case. When a repo IS resolvable, repoErr
// is nil and the check participates in the OK gate normally.
func shellOutCheck(id, name, repoRoot string, repoErr error, argv []string, evidenceOK string) Check {
	return Check{
		ID:       id,
		Name:     name,
		Optional: repoErr != nil,
		Run: func(ctx context.Context) (string, error) {
			if repoErr != nil {
				return "", repoErr
			}
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir = repoRoot
			// Capture stderr+stdout; go vet/test write errors to both.
			var combined bytes.Buffer
			cmd.Stdout = &combined
			cmd.Stderr = &combined
			if err := cmd.Run(); err != nil {
				return tailBytes(combined.String(), 2048), fmt.Errorf("%s failed: %w", strings.Join(argv, " "), err)
			}
			return evidenceOK, nil
		},
	}
}

func tailBytes(s string, n int) string {
	if len(s) <= n {
		return strings.TrimSpace(s)
	}
	return "...\n" + strings.TrimSpace(s[len(s)-n:])
}

// ---- in-process self-tests ----

func checkLedgerSelf(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "verify-ledger-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	l, err := ledger.Open(dir)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	taskID := "verify-task"
	if err := l.Append(taskID, ledger.Entry{Phase: "verify", Action: "self-test", Result: ledger.ResultOK}); err != nil {
		return "", fmt.Errorf("append: %w", err)
	}
	entries, err := l.Read(taskID)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("expected 1 entry, got %d", len(entries))
	}
	return "1 signed entry round-tripped", nil
}

func checkMarkersSelf(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "verify-markers-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	store, err := markers.NewStore(dir)
	if err != nil {
		return "", fmt.Errorf("new store: %w", err)
	}
	m := markers.Marker{Pipeline: "verify", Step: "self", InputHash: "sha256:abc"}
	if err := store.Write(m); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	fresh, err := store.IsFresh("verify", "self", "sha256:abc")
	if err != nil {
		return "", fmt.Errorf("isfresh same: %w", err)
	}
	if !fresh {
		return "", errors.New("expected fresh=true for matching hash")
	}
	stale, err := store.IsFresh("verify", "self", "sha256:different")
	if err != nil {
		return "", fmt.Errorf("isfresh different: %w", err)
	}
	if stale {
		return "", errors.New("expected fresh=false for non-matching hash")
	}
	return "marker fresh on match, stale on mismatch", nil
}

func checkPairingSelf(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "verify-pairing-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	store, err := pairing.NewStore(dir)
	if err != nil {
		return "", fmt.Errorf("new store: %w", err)
	}
	p, err := store.Create("verify-self")
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	authed, err := store.Authenticate(p.Token)
	if err != nil {
		return "", fmt.Errorf("authenticate: %w", err)
	}
	if authed.ID != p.ID {
		return "", fmt.Errorf("authenticate returned wrong id: %s vs %s", authed.ID, p.ID)
	}
	if err := store.Revoke(p.ID); err != nil {
		return "", fmt.Errorf("revoke: %w", err)
	}
	if _, err := store.Authenticate(p.Token); !errors.Is(err, pairing.ErrNotFound) {
		return "", fmt.Errorf("expected ErrNotFound after revoke, got %v", err)
	}
	return "create -> auth -> revoke -> auth-fails", nil
}

func checkBudgetSelf(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "verify-budget-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	store, err := budget.NewStore(dir)
	if err != nil {
		return "", fmt.Errorf("new store: %w", err)
	}
	id := "verify-pair"
	// Cap at 2 requests, $100 cost (default).
	if err := store.SetLimits(id, 2, 0); err != nil {
		return "", fmt.Errorf("set limits: %w", err)
	}
	if err := store.Consume(id, 1); err != nil {
		return "", fmt.Errorf("first consume: %w", err)
	}
	if err := store.Consume(id, 1); err != nil {
		return "", fmt.Errorf("second consume: %w", err)
	}
	err = store.Consume(id, 1)
	if !errors.Is(err, budget.ErrRateLimitExceeded) {
		return "", fmt.Errorf("expected ErrRateLimitExceeded, got %v", err)
	}
	return "cap=2, two ok, third rejected", nil
}

func checkConfigsyncSelf(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "verify-configsync-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	// Build a small source bundle.
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hi"), 0o644); err != nil {
		return "", err
	}

	// Generate a keypair.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("genkey: %w", err)
	}
	signer := configsync.NewSigner(priv)
	bundle := filepath.Join(dir, "bundle.tar")
	if err := signer.PackDir(src, bundle, "verify-self"); err != nil {
		return "", fmt.Errorf("pack: %w", err)
	}

	// Verify with the right key.
	v := configsync.NewVerifier(pub)
	if _, err := v.Verify(bundle); err != nil {
		return "", fmt.Errorf("verify: %w", err)
	}

	// Verify with a different key — must reject.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("genkey other: %w", err)
	}
	bad := configsync.NewVerifier(otherPub)
	if _, err := bad.Verify(bundle); !errors.Is(err, configsync.ErrBadSignature) {
		return "", fmt.Errorf("expected ErrBadSignature with wrong key, got %v", err)
	}
	return "pack -> verify ok, wrong-key -> ErrBadSignature", nil
}

func checkMetricsSelf(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "verify-metrics-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", err
	}
	// Drop one minimally-valid JSONL line so Collect has something to scan.
	ev := map[string]any{
		"ts":          time.Now().UTC().Format(time.RFC3339Nano),
		"node":        "verify",
		"phase":       "verify",
		"action":      "self-test",
		"result":      "ok",
		"duration_ms": 5,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(logDir, "verify.jsonl"), append(line, '\n'), 0o644); err != nil {
		return "", err
	}

	collector := metrics.New(logDir)
	ms, err := collector.Collect()
	if err != nil {
		return "", fmt.Errorf("collect: %w", err)
	}
	if len(ms) == 0 {
		return "", errors.New("collector returned no metrics for a non-empty log")
	}
	var buf bytes.Buffer
	if err := metrics.Render(&buf, ms); err != nil {
		return "", fmt.Errorf("render: %w", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "# HELP") {
		return "", fmt.Errorf("render output missing # HELP prefix; got first line %q", firstLine(out))
	}
	return fmt.Sprintf("%d metric(s), text exposition", len(ms)), nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// SortResultsByID is a small helper for JSON consumers that prefer a
// stable id-sorted order to the input order. Not used by the default
// renderer.
func SortResultsByID(results []Result) {
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
}

