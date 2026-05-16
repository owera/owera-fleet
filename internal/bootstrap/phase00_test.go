// Package bootstrap_test verifies the worker-side bash fragments under
// remote/ in two ways: a static scan of the script source for the
// invariants this repo cares about (shebang, JSONL emitter, canonical
// action names), and a runtime execution of `--dry-run` to confirm the
// script parses + runs under stock macOS bash and emits well-formed
// JSONL on stderr.
//
// The runtime portion is the load-bearing piece of the acceptance test:
// "Re-run reports no-change; shellcheck clean." We run --dry-run twice
// and assert every emitted event is one of {no-change, skipped} —
// nothing in {ok, error, warn} — because dry-run must never mutate.
package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath resolves remote/phase00_brew_baseline.sh relative to this
// test file. The test binary's working directory is the package dir
// (internal/bootstrap), so we walk up to the repo root.
func scriptPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/bootstrap -> repo root is two levels up.
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	p := filepath.Join(root, "remote", "phase00_brew_baseline.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("phase00 script not found at %s: %v", p, err)
	}
	return p
}

func TestPhase00_StaticContract(t *testing.T) {
	p := scriptPath(t)
	src, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	body := string(src)

	// Bash 3.2 binds these fragments; the shebang must be /bin/bash, not
	// /usr/bin/env bash (which would resolve to a Homebrew bash 5 on the
	// gateway and lull us into using bash-4+ syntax).
	if !strings.HasPrefix(body, "#!/bin/bash\n") {
		t.Errorf("phase00: expected shebang '#!/bin/bash', got first line %q",
			strings.SplitN(body, "\n", 2)[0])
	}

	// The canonical JSONL schema fields must all appear in the printf
	// format strings used by the inline emitter.
	for _, field := range []string{"ts", "node", "phase", "action", "result", "duration_ms"} {
		needle := `"` + field + `":`
		if !strings.Contains(body, needle) {
			t.Errorf("phase00: emitter missing JSONL field %q", field)
		}
	}

	// Required literal action names. The per-formula install_<formula>
	// actions are synthesized at runtime ("install_$formula"); we assert
	// the template + the default essentials list separately below.
	for _, action := range []string{
		"brew_present",
		"shellenv_wired",
		"local_bin_dir",
		"summary",
	} {
		if !strings.Contains(body, action) {
			t.Errorf("phase00: required action %q not referenced in script", action)
		}
	}
	if !strings.Contains(body, "install_$formula") {
		t.Error("phase00: dynamic install_<formula> action name missing")
	}
	for _, formula := range []string{"jq", "coreutils", "shellcheck", "gh", "wget", "ripgrep"} {
		if !strings.Contains(body, formula) {
			t.Errorf("phase00: default essentials list missing %q", formula)
		}
	}

	// All four canonical result values must be reachable. (We don't
	// enforce that every result is used in every action — just that the
	// vocabulary the schema permits is present.)
	for _, result := range []string{"no-change", "ok", "skipped", "error"} {
		if !strings.Contains(body, `"`+result+`"`) {
			t.Errorf("phase00: result vocabulary missing %q", result)
		}
	}
}

// event mirrors the subset of internal/log.Event we need to assert on
// in the dry-run trace.
type event struct {
	TS         string `json:"ts"`
	Node       string `json:"node"`
	Phase      string `json:"phase"`
	Action     string `json:"action"`
	Result     string `json:"result"`
	DurationMs int64  `json:"duration_ms"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// runDryRun invokes the script under /bin/bash with --dry-run, captures
// stderr (JSONL channel), and parses every line. Returns the parsed
// events. Skips the test if /bin/bash isn't present (non-macOS dev box).
func runDryRun(t *testing.T) []event {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skipf("phase00 dry-run runtime test is macOS-only (need /bin/bash 3.2); GOOS=%s", runtime.GOOS)
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skipf("/bin/bash not present: %v", err)
	}
	cmd := exec.Command("/bin/bash", scriptPath(t), "--dry-run", "--node", "phase00-test")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Exit 2 (brew missing) is a legitimate environment outcome we
		// shouldn't treat as a test failure — just skip the JSONL
		// assertions, since the script aborts before emitting most
		// actions. shellcheck and the static contract test already
		// cover the source-of-truth.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
			t.Skipf("brew not installed on this host (exit 2); dry-run runtime test needs a brew-baselined host")
		}
		t.Fatalf("dry-run failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	var events []event
	for _, line := range strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid JSONL on stderr: %v\nline: %s", err, line)
		}
		events = append(events, ev)
	}
	return events
}

func TestPhase00_DryRun_NoMutationsAndIdempotent(t *testing.T) {
	events := runDryRun(t)
	if len(events) == 0 {
		t.Fatal("dry-run emitted no JSONL events")
	}

	// Every event must be tagged with our override node + phase.
	for _, ev := range events {
		if ev.Node != "phase00-test" {
			t.Errorf("event node = %q, want phase00-test", ev.Node)
		}
		if ev.Phase != "phase00_brew_baseline" {
			t.Errorf("event phase = %q, want phase00_brew_baseline", ev.Phase)
		}
	}

	// Dry-run must never mutate. Per-action events must be either
	// no-change (state already correct) or skipped (mutation suppressed).
	// The terminal "summary" event is a passive aggregate report — it's
	// allowed to be "ok" because it just rolls up the per-action
	// outcomes, not because the script changed anything.
	for _, ev := range events {
		if ev.Action == "summary" {
			continue
		}
		switch ev.Result {
		case "no-change", "skipped":
			// fine
		default:
			t.Errorf("dry-run emitted non-passive result %q for action %q (stderr_tail=%q)",
				ev.Result, ev.Action, ev.StderrTail)
		}
	}

	// brew_present should always be reported on a brew-baselined host.
	var seenBrewPresent, seenSummary bool
	for _, ev := range events {
		if ev.Action == "brew_present" {
			seenBrewPresent = true
		}
		if ev.Action == "summary" {
			seenSummary = true
		}
	}
	if !seenBrewPresent {
		t.Error("dry-run did not emit a brew_present event")
	}
	if !seenSummary {
		t.Error("dry-run did not emit a summary event")
	}

	// Idempotency: second invocation must produce a structurally
	// identical sequence of (action, result) pairs.
	second := runDryRun(t)
	if len(second) != len(events) {
		t.Fatalf("re-run produced %d events; first run had %d", len(second), len(events))
	}
	for i := range events {
		if events[i].Action != second[i].Action || events[i].Result != second[i].Result {
			t.Errorf("re-run drift at event %d: (%s,%s) -> (%s,%s)",
				i, events[i].Action, events[i].Result, second[i].Action, second[i].Result)
		}
	}
}
