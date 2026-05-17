// phases_static_test.go — static contract verification for the bootstrap
// pipeline's phase01..phase09 fragments. Mirrors the source-of-truth
// shape from phase00_test.go: assert the shebang, the JSONL emitter's
// canonical schema fields, the canonical result vocabulary, and the
// presence of a terminal `summary` action.
//
// Static (source-grep) only — runtime dry-run smoke is deferred to the
// per-phase live tests, since most of these phases need a real worker
// to exercise meaningfully (sudo, dscl, launchctl, ...).
package bootstrap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// phaseScript resolves remote/<name> relative to this test file.
func phaseScript(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	p := filepath.Join(root, "remote", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("phase script not found at %s: %v", p, err)
	}
	return p
}

// allPhaseScripts is the canonical phase01..phase09 list.
var allPhaseScripts = []string{
	"phase01_verify_prereqs.sh",
	"phase02_create_user.sh",
	"phase03_install_hermes.sh",
	"phase04_seed_config.sh",
	"phase05_distribute_secrets.sh",
	"phase06_setup_dedicated_key.sh",
	"phase07_install_tirith.sh",
	"phase08_launchd_heartbeat.sh",
	"phase09_verify.sh",
}

func TestPhasesStaticContract(t *testing.T) {
	for _, name := range allPhaseScripts {
		name := name
		t.Run(name, func(t *testing.T) {
			p := phaseScript(t, name)
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			body := string(src)

			// Bash 3.2 binding: shebang must be /bin/bash, not env bash.
			if !strings.HasPrefix(body, "#!/bin/bash\n") {
				t.Errorf("%s: expected shebang '#!/bin/bash', got first line %q",
					name, strings.SplitN(body, "\n", 2)[0])
			}

			// JSONL schema fields must all appear in the printf format strings.
			for _, field := range []string{"ts", "node", "phase", "action", "result", "duration_ms"} {
				needle := `"` + field + `":`
				if !strings.Contains(body, needle) {
					t.Errorf("%s: emitter missing JSONL field %q", name, field)
				}
			}

			// log_action helper is the universal emit entry point.
			if !strings.Contains(body, "log_action()") {
				t.Errorf("%s: missing log_action() helper", name)
			}

			// Every phase emits a terminal `summary` action.
			if !strings.Contains(body, `"summary"`) {
				t.Errorf("%s: missing 'summary' action emit", name)
			}

			// PHASE identifier must match the file stem (sans .sh).
			expected := strings.TrimSuffix(name, ".sh")
			if !strings.Contains(body, `PHASE="`+expected+`"`) {
				t.Errorf("%s: PHASE constant should be %q", name, expected)
			}

			// Canonical result vocabulary — script must reach for at
			// least one of {no-change, ok, skipped, error}. We assert
			// the trio that should appear in every idempotent phase:
			// no-change (steady-state), and error (failure path).
			for _, result := range []string{"no-change", "error"} {
				if !strings.Contains(body, `"`+result+`"`) {
					t.Errorf("%s: result vocabulary missing %q", name, result)
				}
			}

			// Must accept --node for orchestrator-uniform argument shape.
			if !strings.Contains(body, "--node") {
				t.Errorf("%s: must accept --node flag", name)
			}

			// Must support --dry-run for non-mutating probe path
			// (orchestrator's --dry-run pass-through). phase09 is
			// read-only so its dry-run is a no-op, but the flag must
			// still parse.
			if !strings.Contains(body, "--dry-run") {
				t.Errorf("%s: must accept --dry-run flag", name)
			}

			// No bash-4+ idioms.
			for _, forbidden := range []string{
				"declare -A",        // associative array
				"mapfile",           // bash 4+
				"readarray",         // bash 4+
				"wait -n",           // bash 4.3+
				"#!/usr/bin/env bash", // wrong shebang
			} {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s: contains bash-4+ idiom %q (must be bash 3.2 compatible)", name, forbidden)
				}
			}
		})
	}
}

func TestPhaseList_MatchesFilesystem(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	entries, err := os.ReadDir(filepath.Join(root, "remote"))
	if err != nil {
		t.Fatalf("readdir remote/: %v", err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "phase") && strings.HasSuffix(name, ".sh") {
			have[name] = true
		}
	}
	// phase00 + phase01..phase09 = 10 scripts total.
	want := append([]string{"phase00_brew_baseline.sh"}, allPhaseScripts...)
	for _, name := range want {
		if !have[name] {
			t.Errorf("expected phase script %s not found in remote/", name)
		}
	}
}
