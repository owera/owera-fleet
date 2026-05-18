package usecasetests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/nodes"
	osh "github.com/owera/owera-fleet/internal/ssh"
	"github.com/owera/owera-fleet/internal/state"
)

// fakeSSHRunner is a stub for the SSH dial path used by S1/S2/S7/S8.
type fakeSSHRunner struct {
	stdout string
	stderr string
	exit   int
	runErr error
}

func (f *fakeSSHRunner) Run(_ context.Context, _ string) (osh.Result, error) {
	return osh.Result{Stdout: f.stdout, Stderr: f.stderr, ExitCode: f.exit}, f.runErr
}
func (f *fakeSSHRunner) Close() error { return nil }

// installFakeDial returns a teardown function the test calls in a defer.
func installFakeDial(t *testing.T, runner *fakeSSHRunner, dialErr error) func() {
	t.Helper()
	prev := dialFn
	dialFn = func(_ context.Context, _ osh.Target, _ ...osh.Option) (sshRunner, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return runner, nil
	}
	return func() { dialFn = prev }
}

// installFakeNodes installs a fixed node registry.
func installFakeNodes(t *testing.T, ns []nodes.Node) func() {
	t.Helper()
	prev := nodesLoadFn
	nodesLoadFn = func() (*nodes.Registry, error) {
		return nodes.New(ns), nil
	}
	return func() { nodesLoadFn = prev }
}

// --- S1 -------------------------------------------------------------------

func TestS1Pass(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{{User: "hermes", Host: "claw1.local"}})()
	defer installFakeDial(t, &fakeSSHRunner{stdout: "claw1.local\n 09:00:00 up 1 day, load average: 0.1 0.2 0.3\n", exit: 0}, nil)()
	r := runS1(context.Background(), Options{})
	if r.Status != StatusPass {
		t.Errorf("S1 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS1FailNoLoadAverage(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{{User: "hermes", Host: "claw1.local"}})()
	defer installFakeDial(t, &fakeSSHRunner{stdout: "claw1.local\n", exit: 0}, nil)()
	r := runS1(context.Background(), Options{})
	if r.Status != StatusFail {
		t.Errorf("S1 status = %s, want fail", r.Status)
	}
}

func TestS1FailDial(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{{User: "hermes", Host: "claw1.local"}})()
	defer installFakeDial(t, nil, errors.New("connection refused"))()
	r := runS1(context.Background(), Options{})
	if r.Status != StatusFail {
		t.Errorf("S1 status = %s, want fail", r.Status)
	}
}

func TestS1FailEmptyRegistry(t *testing.T) {
	defer installFakeNodes(t, nil)()
	r := runS1(context.Background(), Options{})
	if r.Status != StatusFail {
		t.Errorf("S1 status = %s, want fail", r.Status)
	}
}

// --- S2 -------------------------------------------------------------------

func TestS2WarnSingleNode(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{{User: "hermes", Host: "claw1.local"}})()
	r := runS2(context.Background(), Options{})
	if r.Status != StatusWarn {
		t.Errorf("S2 status = %s msg = %q, want warn (single-node)", r.Status, r.Message)
	}
}

func TestS2PassMultiNode(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{
		{User: "hermes", Host: "claw1.local"},
		{User: "hermes", Host: "claw2.local"},
	})()
	defer installFakeDial(t, &fakeSSHRunner{stdout: "2026-05-18T00:00:00Z\n", exit: 0}, nil)()
	r := runS2(context.Background(), Options{})
	if r.Status != StatusPass {
		t.Errorf("S2 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS2FailPartial(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{
		{User: "hermes", Host: "claw1.local"},
		{User: "hermes", Host: "claw2.local"},
	})()
	defer installFakeDial(t, nil, errors.New("connection refused"))()
	r := runS2(context.Background(), Options{})
	if r.Status != StatusFail {
		t.Errorf("S2 status = %s, want fail", r.Status)
	}
}

// --- S3 -------------------------------------------------------------------

func installFakeGit(t *testing.T, fn func(repo string, args ...string) (string, error)) func() {
	t.Helper()
	prev := gitRunFn
	gitRunFn = fn
	return func() { gitRunFn = prev }
}

func installFakeHermesReview(t *testing.T, resp string, err error) func() {
	t.Helper()
	prev := hermesReviewFn
	hermesReviewFn = func(_ context.Context, _ string) (string, error) { return resp, err }
	return func() { hermesReviewFn = prev }
}

func TestS3SkipLLMPasses(t *testing.T) {
	defer installFakeGit(t, func(_ string, _ ...string) (string, error) { return "1 file changed", nil })()
	r := runS3(context.Background(), Options{SkipLLM: true, ReviewRepo: "/tmp/x"})
	if r.Status != StatusPass {
		t.Errorf("S3 (skip-llm) status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS3WarnWhenLLMUnavailable(t *testing.T) {
	defer installFakeGit(t, func(_ string, _ ...string) (string, error) { return "1 file changed", nil })()
	defer installFakeHermesReview(t, "", errors.New("hermes binary missing"))()
	r := runS3(context.Background(), Options{ReviewRepo: "/tmp/x"})
	if r.Status != StatusWarn {
		t.Errorf("S3 status = %s msg = %q, want warn", r.Status, r.Message)
	}
}

func TestS3PassWithLLM(t *testing.T) {
	defer installFakeGit(t, func(_ string, _ ...string) (string, error) { return "1 file changed", nil })()
	defer installFakeHermesReview(t, "## Summary\nLooks good.\n", nil)()
	r := runS3(context.Background(), Options{ReviewRepo: "/tmp/x"})
	if r.Status != StatusPass {
		t.Errorf("S3 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS3FailEmptyDiff(t *testing.T) {
	// Empty diff + empty log = no structural sections.
	defer installFakeGit(t, func(_ string, _ ...string) (string, error) { return "", nil })()
	r := runS3(context.Background(), Options{SkipLLM: true, ReviewRepo: "/tmp/x"})
	if r.Status != StatusFail {
		t.Errorf("S3 status = %s, want fail", r.Status)
	}
}

func TestS3FallsBackOnInvalidBase(t *testing.T) {
	calls := []string{}
	defer installFakeGit(t, func(_ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		// First call is rev-parse; fail it to force fallback.
		if args[0] == "rev-parse" {
			return "", errors.New("not a valid object")
		}
		return "1 file changed", nil
	})()
	r := runS3(context.Background(), Options{SkipLLM: true, ReviewRepo: "/tmp/x", ReviewBase: "HEAD~99"})
	if r.Status != StatusPass {
		t.Errorf("S3 status = %s msg = %q, want pass", r.Status, r.Message)
	}
	// The diff call should now use HEAD~1.
	found := false
	for _, c := range calls {
		if strings.Contains(c, "diff HEAD~1") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected fallback to HEAD~1; calls: %v", calls)
	}
}

// --- S4 -------------------------------------------------------------------

func installFakeHermesResearch(t *testing.T, resp string, err error) func() {
	t.Helper()
	prev := hermesResearchFn
	hermesResearchFn = func(_ context.Context, _ string, _ bool) (string, error) { return resp, err }
	return func() { hermesResearchFn = prev }
}

func TestS4SkipLLM(t *testing.T) {
	r := runS4(context.Background(), Options{SkipLLM: true})
	if r.Status != StatusSkip {
		t.Errorf("S4 status = %s, want skip", r.Status)
	}
}

func TestS4PassWithSections(t *testing.T) {
	defer installFakeHermesResearch(t, "## Findings\n…\n## Sources\n- url\n", nil)()
	r := runS4(context.Background(), Options{})
	if r.Status != StatusPass {
		t.Errorf("S4 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS4WarnWhenSectionsMissing(t *testing.T) {
	defer installFakeHermesResearch(t, "some response with no sections", nil)()
	r := runS4(context.Background(), Options{})
	if r.Status != StatusWarn {
		t.Errorf("S4 status = %s, want warn", r.Status)
	}
}

func TestS4FailOnError(t *testing.T) {
	defer installFakeHermesResearch(t, "", errors.New("hermes down"))()
	r := runS4(context.Background(), Options{})
	if r.Status != StatusFail {
		t.Errorf("S4 status = %s, want fail", r.Status)
	}
}

// --- S5 / S6 --------------------------------------------------------------

type fakeStateProbe struct {
	snap *state.Snapshot
	err  error
}

func (f *fakeStateProbe) Collect(_ context.Context) (*state.Snapshot, error) {
	return f.snap, f.err
}

func installFakeStateCollector(t *testing.T, snap *state.Snapshot, err error) func() {
	t.Helper()
	prev := stateCollectorFn
	stateCollectorFn = func(_ string) StateProbe {
		return &fakeStateProbe{snap: snap, err: err}
	}
	return func() { stateCollectorFn = prev }
}

func TestS5PassEveryNodeInSnapshot(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{
		{User: "hermes", Host: "claw1.local"},
		{User: "hermes", Host: "claw2.local"},
	})()
	defer installFakeStateCollector(t, &state.Snapshot{
		Nodes: []state.NodeRow{
			{Node: nodes.Node{User: "hermes", Host: "claw1.local"}},
			{Node: nodes.Node{User: "hermes", Host: "claw2.local"}},
		},
	}, nil)()
	r := runS5(context.Background(), Options{})
	if r.Status != StatusPass {
		t.Errorf("S5 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS5FailMissingNode(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{
		{User: "hermes", Host: "claw1.local"},
		{User: "hermes", Host: "claw2.local"},
	})()
	defer installFakeStateCollector(t, &state.Snapshot{
		Nodes: []state.NodeRow{{Node: nodes.Node{User: "hermes", Host: "claw1.local"}}},
	}, nil)()
	r := runS5(context.Background(), Options{})
	if r.Status != StatusFail {
		t.Errorf("S5 status = %s msg = %q, want fail", r.Status, r.Message)
	}
}

func TestS6PassStableSnapshots(t *testing.T) {
	defer installFakeStateCollector(t, &state.Snapshot{
		Nodes: []state.NodeRow{
			{Node: nodes.Node{User: "hermes", Host: "claw1.local"}},
		},
		LaunchAgents: []state.LaunchAgent{{Label: "com.owera.watchdog", PID: "1234"}},
	}, nil)()
	r := runS6(context.Background(), Options{})
	if r.Status != StatusPass {
		t.Errorf("S6 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

// --- S7 -------------------------------------------------------------------

func TestS7PassEveryNode(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{
		{User: "hermes", Host: "claw1.local"},
	})()
	defer installFakeDial(t, &fakeSSHRunner{stdout: "  1 launchd\n", exit: 0}, nil)()
	r := runS7(context.Background(), Options{})
	if r.Status != StatusPass {
		t.Errorf("S7 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS7FailMissingNode(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{
		{User: "hermes", Host: "claw1.local"},
	})()
	defer installFakeDial(t, nil, errors.New("connection refused"))()
	r := runS7(context.Background(), Options{})
	if r.Status != StatusFail {
		t.Errorf("S7 status = %s, want fail", r.Status)
	}
}

// --- S8 -------------------------------------------------------------------

func TestS8PassWithHashes(t *testing.T) {
	defer installFakeNodes(t, []nodes.Node{
		{User: "hermes", Host: "claw1.local"},
	})()
	defer installFakeDial(t, &fakeSSHRunner{stdout: "abc123\ndef456\n", exit: 0}, nil)()
	r := runS8(context.Background(), Options{})
	if r.Status != StatusPass {
		t.Errorf("S8 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

// --- S9 -------------------------------------------------------------------

type fakeFileInfo struct {
	name    string
	modTime time.Time
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func TestS9PassWithLowerPin(t *testing.T) {
	prevRead := fileReadFn
	fileReadFn = func(_ string) ([]byte, error) { return []byte("v0.13.0\n"), nil }
	defer func() { fileReadFn = prevRead }()
	r := runS9(context.Background(), Options{HermesDir: "/tmp/x"})
	if r.Status != StatusPass {
		t.Errorf("S9 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS9WarnWhenAlreadyAtTarget(t *testing.T) {
	prevRead := fileReadFn
	fileReadFn = func(_ string) ([]byte, error) { return []byte("v0.99.1\n"), nil }
	defer func() { fileReadFn = prevRead }()
	r := runS9(context.Background(), Options{HermesDir: "/tmp/x"})
	if r.Status != StatusWarn {
		t.Errorf("S9 status = %s msg = %q, want warn", r.Status, r.Message)
	}
}

func TestS9PassWhenPinMissing(t *testing.T) {
	prevRead := fileReadFn
	fileReadFn = func(_ string) ([]byte, error) { return nil, fs.ErrNotExist }
	defer func() { fileReadFn = prevRead }()
	r := runS9(context.Background(), Options{HermesDir: "/tmp/x"})
	if r.Status != StatusPass {
		t.Errorf("S9 status = %s msg = %q, want pass (missing pin defaults to v0.0.0)", r.Status, r.Message)
	}
}

// --- S10 ------------------------------------------------------------------

func TestS10PassAllFresh(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	prevStat := fileStatFn
	prevRead := fileReadFn
	defer func() {
		fileStatFn = prevStat
		fileReadFn = prevRead
	}()

	// All markers exist and are fresh.
	fileStatFn = func(p string) (os.FileInfo, error) {
		return fakeFileInfo{name: filepath.Base(p), modTime: now.Add(-time.Minute)}, nil
	}
	freshLine := fmt.Sprintf(`{"ts":"%s","result":"ok"}`, now.Add(-time.Hour).Format(time.RFC3339Nano))
	fileReadFn = func(_ string) ([]byte, error) {
		return []byte(freshLine + "\n"), nil
	}

	r := runS10(context.Background(), Options{HermesDir: "/tmp/x"})
	if r.Status != StatusPass {
		t.Errorf("S10 status = %s msg = %q, want pass", r.Status, r.Message)
	}
}

func TestS10WarnOnMissingMarker(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	prevStat := fileStatFn
	prevRead := fileReadFn
	defer func() {
		fileStatFn = prevStat
		fileReadFn = prevRead
	}()

	fileStatFn = func(_ string) (os.FileInfo, error) { return nil, fs.ErrNotExist }
	fileReadFn = func(_ string) ([]byte, error) { return nil, fs.ErrNotExist }

	r := runS10(context.Background(), Options{HermesDir: "/tmp/x"})
	if r.Status != StatusWarn {
		t.Errorf("S10 status = %s msg = %q, want warn", r.Status, r.Message)
	}
}

func TestS10WarnOnStaleLog(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	prevStat := fileStatFn
	prevRead := fileReadFn
	defer func() {
		fileStatFn = prevStat
		fileReadFn = prevRead
	}()

	fileStatFn = func(p string) (os.FileInfo, error) {
		return fakeFileInfo{name: filepath.Base(p), modTime: now.Add(-time.Minute)}, nil
	}
	// Old log line (older than the 26h window).
	staleLine := fmt.Sprintf(`{"ts":"%s","result":"ok"}`, now.Add(-48*time.Hour).Format(time.RFC3339Nano))
	fileReadFn = func(p string) ([]byte, error) {
		// Marker reads come through fileStatFn, so any fileReadFn call
		// in S10 is for the JSONL logs — return the stale line.
		_ = p
		return []byte(staleLine + "\n"), nil
	}
	r := runS10(context.Background(), Options{HermesDir: "/tmp/x"})
	if r.Status != StatusWarn {
		t.Errorf("S10 status = %s, want warn (stale log)", r.Status)
	}
	if !strings.Contains(r.Message, "stale") {
		t.Errorf("S10 message = %q, want substring 'stale'", r.Message)
	}
}

// --- checkLog / checkMarker helpers ---------------------------------------

func TestCheckLogParseError(t *testing.T) {
	prev := fileReadFn
	fileReadFn = func(_ string) ([]byte, error) {
		return []byte(`{"result":"ok","ts":"not-a-time"}` + "\n"), nil
	}
	defer func() { fileReadFn = prev }()
	r := checkLog("foo", "/tmp/x.jsonl", time.Hour, time.Now())
	if !strings.Contains(r.result, "parse-error") {
		t.Errorf("checkLog result = %q, want parse-error", r.result)
	}
}

func TestCheckLogJSONShape(t *testing.T) {
	now := time.Now()
	prev := fileReadFn
	fileReadFn = func(_ string) ([]byte, error) {
		line, _ := json.Marshal(map[string]any{
			"ts":     now.Add(-time.Minute).Format(time.RFC3339Nano),
			"result": "ok",
		})
		return append(line, '\n'), nil
	}
	defer func() { fileReadFn = prev }()
	r := checkLog("foo", "/tmp/x.jsonl", time.Hour, now)
	if !strings.HasPrefix(r.result, "ok(") {
		t.Errorf("checkLog result = %q, want ok(<age>)", r.result)
	}
}
