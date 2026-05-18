package healthdiff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestParse_GoldenA covers the canonical 3-host snapshot used as the
// "before" side in the golden diff fixture. The assertions exercise
// the parser's derived fields (uptime_days, heartbeat bucketisation,
// disk_root_pct, mem_avail_pct, brew/risk parsing) which were the
// fragile awk one-liners in the bash port.
func TestParse_GoldenA(t *testing.T) {
	snap, err := Parse(mustReadFile(t, filepath.Join("testdata", "a.md")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(snap.Hosts) != 3 {
		t.Fatalf("hosts: got %d want 3 (%v)", len(snap.Hosts), hostNames(snap))
	}
	gw := findHost(t, snap, "gateway: claw3")
	mustEqual(t, gw.Fields["probe_status"], "ok")
	mustEqual(t, gw.Fields["uptime_days"], "27")
	mustEqual(t, gw.Fields["disk_root_pct"], "7")
	mustEqual(t, gw.Fields["disk_root_used"], "12Gi")
	mustEqual(t, gw.Fields["mem_avail_pct"], "4")
	mustEqual(t, gw.Fields["heartbeat_status"], "gateway-expected")
	mustEqual(t, gw.Fields["brew_outdated_count"], "0")
	mustEqual(t, gw.Fields["brew_outdated_list"], "(none)")
	mustEqual(t, gw.Fields["disk_at_risk_count"], "0")
	mustEqual(t, gw.Fields["disk_at_risk_list"], "(none)")

	w1 := findHost(t, snap, "worker: hermes@claw1.local")
	// 47s ago → "fresh" (< 60s)
	mustEqual(t, w1.Fields["heartbeat_status"], "fresh")
	// brew not installed → n/a
	mustEqual(t, w1.Fields["brew_outdated_count"], "n/a")

	w2 := findHost(t, snap, "worker: hermes@claw2.local")
	// Sub-day uptime like "up 3:14" maps to "0" (bash parity).
	mustEqual(t, w2.Fields["uptime_days"], "0")
}

// TestParse_GoldenB exercises the changed/new shapes used by the "after"
// snapshot: brew outdated with a multi-line list, missing heartbeat
// (gone stale), and a brand-new host.
func TestParse_GoldenB(t *testing.T) {
	snap, err := Parse(mustReadFile(t, filepath.Join("testdata", "b.md")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(snap.Hosts) != 3 {
		t.Fatalf("hosts: got %d want 3", len(snap.Hosts))
	}
	gw := findHost(t, snap, "gateway: claw3")
	mustEqual(t, gw.Fields["brew_outdated_count"], "3")
	mustEqual(t, gw.Fields["brew_outdated_list"], "gh,go,ripgrep")
	mustEqual(t, gw.Fields["disk_root_pct"], "8")

	w1 := findHost(t, snap, "worker: hermes@claw1.local")
	mustEqual(t, w1.Fields["heartbeat_status"], "missing")

	w3 := findHost(t, snap, "worker: hermes@claw3.local")
	mustEqual(t, w3.Fields["uptime_days"], "1")
	mustEqual(t, w3.Fields["heartbeat_status"], "fresh")
}

// TestParse_ProbeFailure asserts the "no OS field" short-circuit: the
// parser must emit just probe_status=failed instead of a flurry of
// half-populated fields.
func TestParse_ProbeFailure(t *testing.T) {
	body := `# Fleet health snapshot

## worker: hermes@brokenhost

_probe took 60000 ms; exit=255_

` + "```" + `
SSH_ERROR: timed out after 60s
` + "```" + `
`
	snap, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(snap.Hosts) != 1 {
		t.Fatalf("hosts: got %d want 1", len(snap.Hosts))
	}
	h := snap.Hosts[0]
	if h.Fields["probe_status"] != "failed" {
		t.Errorf("probe_status: got %q want failed", h.Fields["probe_status"])
	}
	if len(h.Fields) != 1 {
		t.Errorf("expected only probe_status, got %d fields: %v", len(h.Fields), h.Fields)
	}
}

// TestDiff_Identical asserts the no-op path: parsing the same snapshot
// against itself produces zero changes (the "exit 0" CI gate case).
func TestDiff_Identical(t *testing.T) {
	body := mustReadFile(t, filepath.Join("testdata", "a.md"))
	a, _ := Parse(body)
	b, _ := Parse(body)
	r := Diff(a, b)
	if r.TotalChanges != 0 {
		t.Errorf("identical diff: got %d changes want 0\n%+v", r.TotalChanges, r.Changes)
	}
	if r.MaxSeverity() != "" {
		t.Errorf("identical max severity: got %q want empty", r.MaxSeverity())
	}
}

// TestDiff_HostRemoved verifies a host disappearing produces a crit-
// severity host_removed record and nothing else for that host.
func TestDiff_HostRemoved(t *testing.T) {
	a := Snapshot{Hosts: []Host{
		{Name: "worker: hermes@claw1.local", Fields: map[string]string{
			"probe_status": "ok", "os": "macOS 26.4",
		}},
		{Name: "worker: hermes@claw2.local", Fields: map[string]string{
			"probe_status": "ok", "os": "macOS 26.5",
		}},
	}}
	b := Snapshot{Hosts: []Host{
		{Name: "worker: hermes@claw1.local", Fields: map[string]string{
			"probe_status": "ok", "os": "macOS 26.4",
		}},
	}}
	r := Diff(a, b)
	if r.TotalChanges != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", r.TotalChanges, r.Changes)
	}
	c := r.Changes[0]
	if c.Kind != KindHostRemoved {
		t.Errorf("kind: got %q want host_removed", c.Kind)
	}
	if c.Severity != SeverityCrit {
		t.Errorf("severity: got %q want crit", c.Severity)
	}
	if r.MaxSeverity() != SeverityCrit {
		t.Errorf("max severity: got %q want crit", r.MaxSeverity())
	}
}

// TestDiff_FieldAddedAndRemoved asserts the per-field added/removed
// shapes, including the (unset) sentinel in the appropriate slot.
func TestDiff_FieldAddedAndRemoved(t *testing.T) {
	a := Snapshot{Hosts: []Host{{
		Name: "h1", Fields: map[string]string{
			"probe_status": "ok",
			"os":           "macOS 26.4",
			"tirith":       "0.3.1",
		},
	}}}
	b := Snapshot{Hosts: []Host{{
		Name: "h1", Fields: map[string]string{
			"probe_status":   "ok",
			"os":             "macOS 26.4",
			"hermes_version": "v0.14.0",
		},
	}}}
	r := Diff(a, b)
	var added, removed *Change
	for i := range r.Changes {
		switch r.Changes[i].Kind {
		case KindAdded:
			c := r.Changes[i]
			added = &c
		case KindRemoved:
			c := r.Changes[i]
			removed = &c
		}
	}
	if added == nil || added.Field != "hermes_version" || added.Old != unsetSentinel {
		t.Errorf("added: got %+v", added)
	}
	if added != nil && added.Severity != SeverityInfo {
		t.Errorf("added severity: got %q want info", added.Severity)
	}
	if removed == nil || removed.Field != "tirith" || removed.New != unsetSentinel {
		t.Errorf("removed: got %+v", removed)
	}
}

// TestDiff_CPULoadDoubled exercises a numeric change on a warn-classed
// field. The bash port records load via uptime_days indirectly; in the
// Go port mem_avail_pct dropping is the closest analogue and is warn.
func TestDiff_MemDoubled(t *testing.T) {
	a := Snapshot{Hosts: []Host{{
		Name: "h1", Fields: map[string]string{
			"probe_status":  "ok",
			"os":            "macOS 26.4",
			"mem_avail_pct": "20",
		},
	}}}
	b := Snapshot{Hosts: []Host{{
		Name: "h1", Fields: map[string]string{
			"probe_status":  "ok",
			"os":            "macOS 26.4",
			"mem_avail_pct": "5",
		},
	}}}
	r := Diff(a, b)
	if r.TotalChanges != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", r.TotalChanges, r.Changes)
	}
	c := r.Changes[0]
	if c.Kind != KindChanged {
		t.Errorf("kind: got %q want changed", c.Kind)
	}
	if c.Severity != SeverityWarn {
		t.Errorf("severity: got %q want warn", c.Severity)
	}
	if r.MaxSeverity() != SeverityWarn {
		t.Errorf("max: got %q want warn", r.MaxSeverity())
	}
}

// TestDiff_ProbeStatusFlip asserts that a host transitioning from ok
// to failed (or vice versa) is crit-severity.
func TestDiff_ProbeStatusFlip(t *testing.T) {
	a := Snapshot{Hosts: []Host{{
		Name: "h1", Fields: map[string]string{
			"probe_status": "ok", "os": "macOS",
		},
	}}}
	b := Snapshot{Hosts: []Host{{
		Name: "h1", Fields: map[string]string{
			"probe_status": "failed",
		},
	}}}
	r := Diff(a, b)
	// Two changes: probe_status ok→failed AND os removed.
	if r.MaxSeverity() != SeverityCrit {
		t.Errorf("max severity: got %q want crit (probe_status flip)", r.MaxSeverity())
	}
	var sawProbeFlip bool
	for _, c := range r.Changes {
		if c.Field == "probe_status" && c.Kind == KindChanged {
			sawProbeFlip = true
			if c.Severity != SeverityCrit {
				t.Errorf("probe_status flip severity: got %q want crit", c.Severity)
			}
		}
	}
	if !sawProbeFlip {
		t.Errorf("did not see probe_status flip change record: %+v", r.Changes)
	}
}

// TestDiff_GoldenAB is the big end-to-end fixture: parse both files,
// diff them, and assert the high-level shape (host_added for claw3.local,
// host_removed for claw2.local, brew/disk/heartbeat changes on claw3
// gateway, heartbeat going missing on claw1).
func TestDiff_GoldenAB(t *testing.T) {
	a, err := Parse(mustReadFile(t, filepath.Join("testdata", "a.md")))
	if err != nil {
		t.Fatalf("parse a: %v", err)
	}
	b, err := Parse(mustReadFile(t, filepath.Join("testdata", "b.md")))
	if err != nil {
		t.Fatalf("parse b: %v", err)
	}
	r := Diff(a, b)
	r.FromName = "fleet-snapshot-20260515T215500Z.md"
	r.ToName = "fleet-snapshot-20260515T220009Z.md"

	if r.TotalChanges == 0 {
		t.Fatalf("expected non-empty diff")
	}
	if r.MaxSeverity() != SeverityCrit {
		t.Errorf("max severity: got %q want crit (host_removed for claw2)", r.MaxSeverity())
	}

	// Verify the host-level adds/removes
	gotAdded := changesOfKind(r, KindHostAdded)
	gotRemoved := changesOfKind(r, KindHostRemoved)
	if len(gotAdded) != 1 || gotAdded[0].Host != "worker: hermes@claw3.local" {
		t.Errorf("host_added: got %+v", gotAdded)
	}
	if len(gotRemoved) != 1 || gotRemoved[0].Host != "worker: hermes@claw2.local" {
		t.Errorf("host_removed: got %+v", gotRemoved)
	}

	// Verify brew_outdated_count changed on the gateway.
	found := false
	for _, c := range r.Changes {
		if c.Host == "gateway: claw3" && c.Field == "brew_outdated_count" && c.Kind == KindChanged {
			if c.Old != "0" || c.New != "3" {
				t.Errorf("brew change: got %s → %s want 0 → 3", c.Old, c.New)
			}
			if c.Severity != SeverityWarn {
				t.Errorf("brew severity: got %q want warn", c.Severity)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("did not find brew_outdated_count change on gateway")
	}

	// Verify heartbeat going missing on claw1.
	found = false
	for _, c := range r.Changes {
		if c.Host == "worker: hermes@claw1.local" && c.Field == "heartbeat_status" {
			if c.Old != "fresh" || c.New != "missing" {
				t.Errorf("heartbeat: got %s → %s", c.Old, c.New)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("did not find heartbeat change on claw1")
	}
}

// TestRenderMarkdown_Snapshot covers the empty-diff text and the
// per-host bullet shape (one snapshot of golden A vs itself; one of
// A vs B). Asserting on the rendered body keeps the format stable so
// downstream operator dashboards don't break.
func TestRenderMarkdown_NoChanges(t *testing.T) {
	body := mustReadFile(t, filepath.Join("testdata", "a.md"))
	a, _ := Parse(body)
	r := Diff(a, a)
	r.FromName = "fleet-snapshot-20260515T215500Z.md"
	r.ToName = "fleet-snapshot-20260515T215500Z.md"
	md := RenderMarkdown(r)
	if !strings.Contains(md, "_No changes between the two snapshots._") {
		t.Errorf("expected empty-diff text, got:\n%s", md)
	}
}

func TestRenderMarkdown_HasHostSection(t *testing.T) {
	a, _ := Parse(mustReadFile(t, filepath.Join("testdata", "a.md")))
	b, _ := Parse(mustReadFile(t, filepath.Join("testdata", "b.md")))
	r := Diff(a, b)
	r.FromName = "fleet-snapshot-20260515T215500Z.md"
	r.ToName = "fleet-snapshot-20260515T220009Z.md"
	md := RenderMarkdown(r)
	if !strings.Contains(md, "## gateway: claw3") {
		t.Errorf("missing gateway section:\n%s", md)
	}
	if !strings.Contains(md, "**NEW HOST**") {
		t.Errorf("missing NEW HOST marker:\n%s", md)
	}
	if !strings.Contains(md, "**REMOVED HOST**") {
		t.Errorf("missing REMOVED HOST marker:\n%s", md)
	}
	if !strings.Contains(md, "[BREW] brew_outdated_count") {
		t.Errorf("missing brew bullet:\n%s", md)
	}
	if !strings.Contains(md, "_(2026-05-15 21:55:00Z)_") {
		t.Errorf("missing humanised From brief:\n%s", md)
	}
}

func TestRenderJSON_RoundTrip(t *testing.T) {
	a, _ := Parse(mustReadFile(t, filepath.Join("testdata", "a.md")))
	b, _ := Parse(mustReadFile(t, filepath.Join("testdata", "b.md")))
	r := Diff(a, b)
	r.FromName = "from.md"
	r.ToName = "to.md"
	raw, err := RenderJSON(r)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if raw[len(raw)-1] != '\n' {
		t.Errorf("RenderJSON output not newline-terminated")
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v\nraw=%s", err, raw)
	}
	if back.TotalChanges != r.TotalChanges {
		t.Errorf("total_changes round-trip: got %d want %d", back.TotalChanges, r.TotalChanges)
	}
	if back.FromName != "from.md" || back.ToName != "to.md" {
		t.Errorf("from/to round-trip: got %q/%q", back.FromName, back.ToName)
	}
	// generated_at should be populated by the renderer.
	if back.GeneratedAt == "" {
		t.Errorf("generated_at not populated")
	}
}

func TestBriefForFilename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"fleet-snapshot-20260515T220009Z.md", "2026-05-15 22:00:09Z"},
		{"path/to/fleet-snapshot-20260515T220009Z.md", "2026-05-15 22:00:09Z"},
		{"some-other-file.md", "some-other-file.md"},
		{"", ""},
	}
	for _, c := range cases {
		got := briefForFilename(c.in)
		if got != c.want {
			t.Errorf("briefForFilename(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestFieldGroup(t *testing.T) {
	cases := map[string]string{
		"os":                  "STABLE",
		"hermes_version":      "STABLE",
		"disk_root_pct":       "DISK",
		"disk_at_risk_count":  "DISK",
		"mem_avail_pct":       "MEM",
		"heartbeat_status":    "HEARTBEAT",
		"brew_outdated_count": "BREW",
		"uptime_days":         "UPTIME",
		"probe_status":        "INFO",
		"unknown_field":       "INFO",
	}
	for k, want := range cases {
		if got := FieldGroup(k); got != want {
			t.Errorf("FieldGroup(%q): got %q want %q", k, got, want)
		}
	}
}

// --- helpers --------------------------------------------------------------

func hostNames(s Snapshot) []string {
	out := make([]string, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		out = append(out, h.Name)
	}
	return out
}

func findHost(t *testing.T, s Snapshot, name string) Host {
	t.Helper()
	for _, h := range s.Hosts {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("host %q not found in %v", name, hostNames(s))
	return Host{}
}

func mustEqual(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func changesOfKind(r Report, k ChangeKind) []Change {
	var out []Change
	for _, c := range r.Changes {
		if c.Kind == k {
			out = append(out, c)
		}
	}
	return out
}
