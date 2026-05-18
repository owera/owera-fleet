package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFakeRestic stubs `restic` on PATH with a script we control.
// The script writes its invocation args to argsFile and exits with
// the matching exit code from the subcmd→exit map.
//
// argsFile contains one line per invocation: "<subcommand> <args...>"
//
// Use this to capture how `fleetctl backup` invokes restic without
// touching a real repository.
func withFakeRestic(t *testing.T, exitForSubcmd map[string]int) (string, func()) {
	t.Helper()
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args.log")
	// Each subcmd "snapshots"|"init"|"backup"|"forget" → exit code.
	// Use a shell script so we can record + exit dynamically.
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString(fmt.Sprintf("echo \"$@\" >> %q\n", argsFile))
	script.WriteString("case \"$1\" in\n")
	for sub, code := range exitForSubcmd {
		script.WriteString(fmt.Sprintf("  %s) exit %d ;;\n", sub, code))
	}
	script.WriteString("  *) exit 0 ;;\nesac\n")

	resticPath := filepath.Join(bin, "restic")
	if err := os.WriteFile(resticPath, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("write fake restic: %v", err)
	}
	prevPath := os.Getenv("PATH")
	t.Setenv("PATH", bin+":"+prevPath)
	return argsFile, func() {
		t.Setenv("PATH", prevPath)
	}
}

func readArgsFile(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read args file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func TestRunResticBackup_BuildsArgsAndPropagatesError(t *testing.T) {
	// Subcommand "backup" returns 0 → success.
	argsFile, restore := withFakeRestic(t, map[string]int{"backup": 0})
	defer restore()
	// We bypass runBackup wrapper and call runResticBackup directly so
	// we don't need the full env (RESTIC_REPOSITORY etc.) wired.
	prevExec := execCommandContext
	defer func() { execCommandContext = prevExec }()
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Force the fake to be picked up via PATH (the seam is here
		// only for tests that need to *replace* the Cmd; the default
		// goes through exec.LookPath which honours t.Setenv("PATH")).
		return exec.CommandContext(ctx, name, args...)
	}

	logger := newTestLogger(t)
	tagArgs := []string{"--tag", "hermes-state", "--tag", "host=claw3"}
	if err := runResticBackup(context.Background(), "/some/src", tagArgs, false, logger); err != nil {
		t.Fatalf("runResticBackup: %v", err)
	}

	got := readArgsFile(t, argsFile)
	if len(got) != 1 {
		t.Fatalf("invocations: got %d want 1 (%v)", len(got), got)
	}
	// Expect: backup --tag hermes-state --tag host=claw3 --exclude=sandboxes ... /some/src
	line := got[0]
	for _, want := range []string{
		"backup",
		"--tag hermes-state",
		"--tag host=claw3",
		"--exclude=sandboxes",
		"--exclude=cache",
		"--exclude=state.db-wal",
		"--exclude=logs/owera-backup.jsonl",
		"/some/src",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("restic args missing %q in:\n%s", want, line)
		}
	}
	if strings.Contains(line, "--dry-run") {
		t.Errorf("dry-run flag leaked into non-dry call: %s", line)
	}
}

func TestRunResticBackup_DryRunFlag(t *testing.T) {
	argsFile, restore := withFakeRestic(t, map[string]int{"backup": 0})
	defer restore()

	logger := newTestLogger(t)
	if err := runResticBackup(context.Background(), "/src", []string{"--tag", "x"}, true, logger); err != nil {
		t.Fatalf("runResticBackup: %v", err)
	}
	line := readArgsFile(t, argsFile)[0]
	if !strings.Contains(line, "--dry-run") {
		t.Errorf("dry-run flag missing: %s", line)
	}
}

func TestRunResticBackup_PropagatesNonZeroExit(t *testing.T) {
	_, restore := withFakeRestic(t, map[string]int{"backup": 3})
	defer restore()

	logger := newTestLogger(t)
	err := runResticBackup(context.Background(), "/src", []string{"--tag", "x"}, false, logger)
	if err == nil {
		t.Fatal("expected non-nil error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "restic backup") {
		t.Errorf("error message: got %q want contains 'restic backup'", err.Error())
	}
}

func TestEnsureResticRepo_NoopWhenSnapshotsSucceeds(t *testing.T) {
	argsFile, restore := withFakeRestic(t, map[string]int{"snapshots": 0})
	defer restore()

	logger := newTestLogger(t)
	if err := ensureResticRepo(context.Background(), logger); err != nil {
		t.Fatalf("ensureResticRepo: %v", err)
	}
	got := readArgsFile(t, argsFile)
	if len(got) != 1 {
		t.Fatalf("invocations: got %d want 1 (only snapshots probe)", len(got))
	}
	if !strings.Contains(got[0], "snapshots") {
		t.Errorf("first call should be snapshots probe: %q", got[0])
	}
}

func TestEnsureResticRepo_InitsWhenSnapshotsFails(t *testing.T) {
	argsFile, restore := withFakeRestic(t, map[string]int{"snapshots": 1, "init": 0})
	defer restore()

	logger := newTestLogger(t)
	if err := ensureResticRepo(context.Background(), logger); err != nil {
		t.Fatalf("ensureResticRepo: %v", err)
	}
	got := readArgsFile(t, argsFile)
	if len(got) != 2 {
		t.Fatalf("invocations: got %d want 2 (snapshots+init), %v", len(got), got)
	}
}

func TestWriteLastBackupMarker_FormatsTimestamp(t *testing.T) {
	dir := t.TempDir()
	at, err := time.Parse(time.RFC3339, "2026-05-18T13:15:08Z")
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	if err := writeLastBackupMarker(dir, at); err != nil {
		t.Fatalf("writeLastBackupMarker: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "LAST_BACKUP"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	want := "2026-05-18T13:15:08Z\n"
	if string(body) != want {
		t.Errorf("marker contents: got %q want %q", string(body), want)
	}
}

func TestResticExcludesAreStableAndComplete(t *testing.T) {
	// Golden: any change here is a deliberate decision; touch this
	// test alongside the bash-version excludes if you change the
	// canonical set.
	want := map[string]bool{
		"sandboxes":                       true,
		"cache":                           true,
		"*.bak.*":                         true,
		"models_dev_cache*":               true,
		"state.db-shm":                    true,
		"state.db-wal":                    true,
		"logs/backup.log":                 true,
		"logs/owera-backup.jsonl":         true,
		"logs/owera-backup.launchd.out":   true,
		"logs/owera-backup.launchd.err":   true,
	}
	for _, ex := range resticExcludes {
		if !want[ex] {
			t.Errorf("unexpected exclude in canonical set: %q", ex)
		}
		delete(want, ex)
	}
	for missing := range want {
		t.Errorf("missing canonical exclude: %q", missing)
	}
}

func TestTailLines_Truncates(t *testing.T) {
	short := strings.Repeat("x", 50)
	if got := tailLines(short, 100); got != short {
		t.Errorf("short: got len=%d want pass-through", len(got))
	}
	long := strings.Repeat("y", 5000)
	got := tailLines(long, 1000)
	if len(got) != 1000 {
		t.Errorf("long: got len=%d want 1000", len(got))
	}
}

func TestShortHostname_StripsDomain(t *testing.T) {
	// We can't change os.Hostname output, but we can at least confirm
	// shortHostname returns a non-empty string with no dots.
	h := shortHostname()
	if h == "" {
		t.Fatal("shortHostname returned empty string")
	}
	if strings.Contains(h, ".") {
		t.Errorf("hostname contains dot: %q", h)
	}
}

