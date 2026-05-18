package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/nodes"
)

// withFakeRsync stubs `rsync` on PATH with a script that records its
// args + exits with subcmd-independent codes per-target. We key the
// exit code by the destination filename (last arg) so each test can
// drive different per-worker outcomes.
func withFakeRsync(t *testing.T, exitByDestSuffix map[string]int, defaultExit int) (string, func()) {
	t.Helper()
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args.log")
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString(fmt.Sprintf("echo \"$@\" >> %q\n", argsFile))
	// POSIX-portable "last positional arg": iterate, retain. Bash's
	// ${@: -1} is unsupported in dash (the /bin/sh on Ubuntu CI).
	script.WriteString("dest=\"\"\nfor a in \"$@\"; do dest=\"$a\"; done\n")
	script.WriteString("case \"$dest\" in\n")
	for suffix, code := range exitByDestSuffix {
		script.WriteString(fmt.Sprintf("  *%s*) exit %d ;;\n", suffix, code))
	}
	script.WriteString(fmt.Sprintf("  *) exit %d ;;\n", defaultExit))
	script.WriteString("esac\n")
	rsyncPath := filepath.Join(bin, "rsync")
	if err := os.WriteFile(rsyncPath, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("write fake rsync: %v", err)
	}
	prevPath := os.Getenv("PATH")
	t.Setenv("PATH", bin+":"+prevPath)
	return argsFile, func() {
		t.Setenv("PATH", prevPath)
	}
}

func TestPullOneWorker_BuildsExpectedArgs(t *testing.T) {
	argsFile, restore := withFakeRsync(t, nil, 0)
	defer restore()

	destRoot := t.TempDir()
	n := nodes.Node{User: "hermes", Host: "claw1.local"}
	err := pullOneWorker(context.Background(), n, destRoot, 10*time.Second, 0, false)
	if err != nil {
		t.Fatalf("pullOneWorker: %v", err)
	}
	body, _ := os.ReadFile(argsFile)
	line := strings.TrimSpace(string(body))
	for _, want := range []string{
		"-az",
		"--delete",
		"--partial",
		"--inplace",
		"-e",
		"ssh -o BatchMode=yes -o ConnectTimeout=10",
		"--exclude=sandboxes",
		"--exclude=cache",
		"--exclude=hermes-agent",
		"--exclude=node",
		"--exclude=bin",
		"--exclude=logs",
		"hermes@claw1.local:/Users/hermes/.hermes/",
		filepath.Join(destRoot, "claw1.local") + "/",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("rsync args missing %q in:\n%s", want, line)
		}
	}
	if strings.Contains(line, "--dry-run") {
		t.Errorf("dry-run leaked: %s", line)
	}
	if strings.Contains(line, "--bwlimit") {
		t.Errorf("bwlimit leaked: %s", line)
	}
}

func TestPullOneWorker_DryRunAndBwLimit(t *testing.T) {
	argsFile, restore := withFakeRsync(t, nil, 0)
	defer restore()

	destRoot := t.TempDir()
	n := nodes.Node{User: "hermes", Host: "claw1.local"}
	if err := pullOneWorker(context.Background(), n, destRoot, 5*time.Second, 4096, true); err != nil {
		t.Fatalf("pullOneWorker: %v", err)
	}
	body, _ := os.ReadFile(argsFile)
	line := strings.TrimSpace(string(body))
	for _, want := range []string{"--dry-run", "-i", "--bwlimit=4096"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: %s", want, line)
		}
	}
}

func TestPullOneWorker_NonZeroReturnsError(t *testing.T) {
	_, restore := withFakeRsync(t, map[string]int{"claw1.local": 23}, 0)
	defer restore()

	destRoot := t.TempDir()
	n := nodes.Node{User: "hermes", Host: "claw1.local"}
	err := pullOneWorker(context.Background(), n, destRoot, 5*time.Second, 0, false)
	if err == nil {
		t.Fatal("expected non-nil error on non-zero rsync exit")
	}
	if !strings.Contains(err.Error(), "rsync") {
		t.Errorf("error: got %q want contains 'rsync'", err.Error())
	}
}

func TestResolveBackupWorkerTargets_NoFilterReturnsAll(t *testing.T) {
	dir := t.TempDir()
	nodesPath := filepath.Join(dir, "nodes.txt")
	body := "# comment\nhermes@claw1.local\nhermes@claw2.local\n"
	if err := os.WriteFile(nodesPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	got, err := resolveBackupWorkerTargets(nodesPath, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len: got %d want 2 (%v)", len(got), got)
	}
}

func TestResolveBackupWorkerTargets_FilterByExactMatch(t *testing.T) {
	dir := t.TempDir()
	nodesPath := filepath.Join(dir, "nodes.txt")
	body := "hermes@claw1.local\nhermes@claw2.local\n"
	if err := os.WriteFile(nodesPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	got, err := resolveBackupWorkerTargets(nodesPath, []string{"hermes@claw2.local"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("filter: got %d want 1 (%v)", len(got), got)
	}
	if got[0].Host != "claw2.local" {
		t.Errorf("filter target: got %q want claw2.local", got[0].Host)
	}
}

func TestRsyncExcludesAreStableAndComplete(t *testing.T) {
	want := map[string]bool{
		"sandboxes":         true,
		"cache":             true,
		"*.bak.*":           true,
		"models_dev_cache*": true,
		"state.db-shm":      true,
		"state.db-wal":      true,
		"logs":              true,
		"hermes-agent":      true,
		"node":              true,
		"bin":               true,
	}
	for _, ex := range rsyncExcludes {
		if !want[ex] {
			t.Errorf("unexpected exclude: %q", ex)
		}
		delete(want, ex)
	}
	for missing := range want {
		t.Errorf("missing canonical exclude: %q", missing)
	}
}
