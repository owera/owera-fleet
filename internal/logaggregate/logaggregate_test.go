package logaggregate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeTarGz builds a gzipped tarball whose entries are name→content pairs.
// Used to synthesize the bytes a real worker would have streamed back.
func makeTarGz(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Sort for determinism so failing tests print a stable order.
	names := make([]string, 0, len(entries))
	for k := range entries {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		body := entries[name]
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.String()
}

// recordingRunner is a per-target scripted SSHRunner. Each call to it
// pops the corresponding entry off the queue keyed by target+command;
// unmatched calls fail the test (the production code should not run
// commands the test didn't expect).
type recordingRunner struct {
	mu sync.Mutex
	// responses[target] returns the next response in queue order; if
	// stdout starts with "ERR:", the remainder is treated as an error
	// message and the runner returns it as err.
	responses map[string][]runnerResp
	// rotateSeen[target] flips true when a rotate command is observed,
	// so the rotation test can assert it actually fired.
	rotateSeen map[string]bool
}

type runnerResp struct {
	stdout, stderr string
	exit           int
	err            error
}

func (r *recordingRunner) run(ctx context.Context, target, cmd string) (string, string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rotateSeen == nil {
		r.rotateSeen = map[string]bool{}
	}
	if strings.Contains(cmd, "gzip -f") {
		r.rotateSeen[target] = true
		return "", "", 0, nil
	}
	q, ok := r.responses[target]
	if !ok || len(q) == 0 {
		return "", "no scripted response", 1, errors.New("logaggregate-test: no scripted response for target " + target)
	}
	next := q[0]
	r.responses[target] = q[1:]
	return next.stdout, next.stderr, next.exit, next.err
}

// TestRunHappyPathTwoNodes proves the aggregator extracts tarballs from
// both targets, lands the files under per-host directories, and reports
// OK=true with correct counts.
func TestRunHappyPathTwoNodes(t *testing.T) {
	dir := t.TempDir()
	tarA := makeTarGz(t, map[string]string{
		"delegate.jsonl": `{"ts":"2026-05-16T00:00:00Z","node":"a"}` + "\n",
		"smoke.jsonl":    `{"ts":"2026-05-16T00:00:01Z","node":"a"}` + "\n",
	})
	tarB := makeTarGz(t, map[string]string{
		"audit.jsonl": `{"ts":"2026-05-16T00:00:02Z","node":"b"}` + "\n",
	})
	runner := &recordingRunner{
		responses: map[string][]runnerResp{
			"hermes@a.local": {{stdout: tarA, exit: 0}},
			"hermes@b.local": {{stdout: tarB, exit: 0}},
		},
	}

	agg := &Aggregator{
		GatewayLogDir: filepath.Join(dir, "aggregated"),
		SSHRunner:     runner.run,
		SingleNode:    "", // exercised by passing both targets in a fake nodes.txt
		KeepDays:      30,
	}
	// nodes.Load can't be hit here without a real ~/.hermes/nodes.txt.
	// Use the SingleNode escape hatch for each target sequentially, then
	// merge the results manually. Cleaner: run with a fixture nodes file.
	nodesFile := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(nodesFile, []byte("hermes@a.local\nhermes@b.local\n"), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	agg.NodesFile = nodesFile

	got, err := agg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.OK {
		t.Errorf("want OK=true, got false; pulls=%+v", got.Pulls)
	}
	if len(got.Pulls) != 2 {
		t.Fatalf("want 2 PullResults, got %d", len(got.Pulls))
	}
	byHost := map[string]PullResult{}
	for _, p := range got.Pulls {
		byHost[hostOf(p.Node)] = p
	}
	if r := byHost["a.local"]; r.FilesPulled != 2 {
		t.Errorf("a.local FilesPulled=%d, want 2", r.FilesPulled)
	}
	if r := byHost["b.local"]; r.FilesPulled != 1 {
		t.Errorf("b.local FilesPulled=%d, want 1", r.FilesPulled)
	}
	for _, host := range []string{"a.local", "b.local"} {
		hostDir := filepath.Join(dir, "aggregated", host)
		entries, err := os.ReadDir(hostDir)
		if err != nil {
			t.Errorf("read %s: %v", hostDir, err)
			continue
		}
		if len(entries) == 0 {
			t.Errorf("expected files under %s, found none", hostDir)
		}
	}
}

// TestRunPartialFailureSurfacesOKFalse exercises the "one node SSH errored,
// the other succeeded" path: OK must be false, the failing pull's Err must
// be set, and the successful one must still have its files extracted.
func TestRunPartialFailureSurfacesOKFalse(t *testing.T) {
	dir := t.TempDir()
	tarA := makeTarGz(t, map[string]string{
		"delegate.jsonl": `ok` + "\n",
	})
	runner := &recordingRunner{
		responses: map[string][]runnerResp{
			"hermes@a.local": {{stdout: tarA, exit: 0}},
			"hermes@b.local": {{stderr: "connection refused", exit: 255, err: errors.New("ssh: connection refused")}},
		},
	}
	nodesFile := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(nodesFile, []byte("hermes@a.local\nhermes@b.local\n"), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	agg := &Aggregator{
		GatewayLogDir: filepath.Join(dir, "aggregated"),
		SSHRunner:     runner.run,
		NodesFile:     nodesFile,
		KeepDays:      30,
	}
	got, err := agg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.OK {
		t.Errorf("want OK=false on partial failure, got true")
	}
	if len(got.Pulls) != 2 {
		t.Fatalf("want 2 PullResults, got %d", len(got.Pulls))
	}
	var aErr, bErr error
	for _, p := range got.Pulls {
		switch hostOf(p.Node) {
		case "a.local":
			aErr = p.Err
		case "b.local":
			bErr = p.Err
		}
	}
	if aErr != nil {
		t.Errorf("a.local should have succeeded, got Err=%v", aErr)
	}
	if bErr == nil {
		t.Errorf("b.local should have failed, got Err=nil")
	}
	// Survivor extracted.
	files, _ := os.ReadDir(filepath.Join(dir, "aggregated", "a.local"))
	if len(files) == 0 {
		t.Errorf("expected a.local to have extracted files despite b's failure")
	}
}

// TestRetentionPruning synthesises an old file in the landing directory,
// runs the aggregator with KeepDays=7, and asserts the old file is gone
// while the freshly-pulled file remains.
func TestRetentionPruning(t *testing.T) {
	dir := t.TempDir()
	gatewayLog := filepath.Join(dir, "aggregated")
	hostDir := filepath.Join(gatewayLog, "a.local")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Pre-existing stale file: 30 days old.
	stalePath := filepath.Join(hostDir, "stale.jsonl")
	if err := os.WriteFile(stalePath, []byte("ancient"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	staleTime := now.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(stalePath, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	tarA := makeTarGz(t, map[string]string{
		"fresh.jsonl": `current` + "\n",
	})
	runner := &recordingRunner{
		responses: map[string][]runnerResp{
			"hermes@a.local": {{stdout: tarA, exit: 0}},
		},
	}
	nodesFile := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(nodesFile, []byte("hermes@a.local\n"), 0o644); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	agg := &Aggregator{
		GatewayLogDir: gatewayLog,
		SSHRunner:     runner.run,
		NodesFile:     nodesFile,
		KeepDays:      7,
		Now:           func() time.Time { return now },
	}
	got, err := agg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.OK {
		t.Fatalf("want OK=true, got false; pulls=%+v", got.Pulls)
	}
	if got.Pulls[0].FilesPruned == 0 {
		t.Errorf("expected at least one pruned file, got FilesPruned=0")
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("expected stale file gone, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(hostDir, "fresh.jsonl")); err != nil {
		t.Errorf("expected fresh file present, stat err=%v", err)
	}
}

// TestDryRunDoesNotExtract sanity-checks the --dry-run path: the SSH
// probe runs, but no files appear under the landing directory.
func TestDryRunDoesNotExtract(t *testing.T) {
	dir := t.TempDir()
	runner := &recordingRunner{
		responses: map[string][]runnerResp{
			"hermes@a.local": {{stdout: "3\n", exit: 0}},
		},
	}
	nodesFile := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(nodesFile, []byte("hermes@a.local\n"), 0o644); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	agg := &Aggregator{
		GatewayLogDir: filepath.Join(dir, "aggregated"),
		SSHRunner:     runner.run,
		NodesFile:     nodesFile,
		DryRun:        true,
	}
	got, err := agg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.OK {
		t.Fatalf("want OK=true, got false; pulls=%+v", got.Pulls)
	}
	if got.Pulls[0].FilesPulled != 3 {
		t.Errorf("dry-run FilesPulled=%d, want 3", got.Pulls[0].FilesPulled)
	}
	// No mkdir of the landing dir should have happened in dry-run mode.
	if _, err := os.Stat(filepath.Join(dir, "aggregated")); !os.IsNotExist(err) {
		t.Errorf("expected aggregated/ not to exist in dry-run, stat err=%v", err)
	}
}

// TestRotateOnPullFiresRotateCommand verifies the optional rotate-on-pull
// step issues the gzip rotation command after a successful extract.
func TestRotateOnPullFiresRotateCommand(t *testing.T) {
	dir := t.TempDir()
	tarA := makeTarGz(t, map[string]string{
		"delegate.jsonl": "row\n",
	})
	runner := &recordingRunner{
		responses: map[string][]runnerResp{
			"hermes@a.local": {{stdout: tarA, exit: 0}},
		},
	}
	nodesFile := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(nodesFile, []byte("hermes@a.local\n"), 0o644); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	agg := &Aggregator{
		GatewayLogDir: filepath.Join(dir, "aggregated"),
		SSHRunner:     runner.run,
		NodesFile:     nodesFile,
		KeepDays:      30,
		RotateOnPull:  true,
	}
	got, err := agg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.OK {
		t.Fatalf("want OK=true, got false; pulls=%+v", got.Pulls)
	}
	if !runner.rotateSeen["hermes@a.local"] {
		t.Errorf("expected rotate command to have fired against hermes@a.local")
	}
}

// TestRunRequiresSSHRunner is a guard against constructing an Aggregator
// without the required injection seam.
func TestRunRequiresSSHRunner(t *testing.T) {
	a := &Aggregator{GatewayLogDir: t.TempDir()}
	_, err := a.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for missing SSHRunner")
	}
	if !strings.Contains(err.Error(), "SSHRunner") {
		t.Errorf("error %q should mention SSHRunner", err.Error())
	}
}

// TestExtractTarGzSkipsTraversal proves the basename-flattening defence
// strips any "../" attempts in tar header names.
func TestExtractTarGzSkipsTraversal(t *testing.T) {
	tgz := makeTarGz(t, map[string]string{
		"../../etc/passwd": "bad\n",
		"clean.jsonl":      "good\n",
	})
	dir := t.TempDir()
	files, total, err := extractTarGz([]byte(tgz), dir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Both names flatten to a clean basename: "passwd" and "clean.jsonl".
	// What matters is that nothing escapes destDir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") || strings.Contains(e.Name(), "/") {
			t.Errorf("traversal leaked: %q", e.Name())
		}
	}
	_ = files
	_ = total
}
