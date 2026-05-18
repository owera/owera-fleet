package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	osh "github.com/owera/owera-fleet/internal/ssh"
)

// fakeBrewClient is a stand-in for ssh.RemoteClient — captures the
// commands run and returns canned outputs. Mirrors the watchdog test
// fixtures' style.
type fakeBrewClient struct {
	stdoutByCmd map[string]string // matched by substring on cmd
	stderrByCmd map[string]string
	exitByCmd   map[string]int
	runErr      error
	calls       []string
}

func (f *fakeBrewClient) Run(_ context.Context, cmd string) (osh.Result, error) {
	f.calls = append(f.calls, cmd)
	if f.runErr != nil {
		return osh.Result{}, f.runErr
	}
	stdout := f.stdoutByCmd[matchKey(cmd, f.stdoutByCmd)]
	stderr := f.stderrByCmd[matchKey(cmd, f.stderrByCmd)]
	exit := f.exitByCmd[matchKeyInt(cmd, f.exitByCmd)]
	return osh.Result{Stdout: stdout, Stderr: stderr, ExitCode: exit}, nil
}

func (f *fakeBrewClient) Close() error { return nil }

// matchKey picks the longest key in m that is a substring of cmd, so
// tests can key by a stable fragment (e.g. "brew --version" or "install").
// Falls back to "" (which lets callers register a default).
func matchKey(cmd string, m map[string]string) string {
	best := ""
	for k := range m {
		if k == "" {
			continue
		}
		if strings.Contains(cmd, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		if _, ok := m[""]; ok {
			return ""
		}
	}
	return best
}

func matchKeyInt(cmd string, m map[string]int) string {
	best := ""
	for k := range m {
		if k == "" {
			continue
		}
		if strings.Contains(cmd, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		if _, ok := m[""]; ok {
			return ""
		}
	}
	return best
}

// fakeBrewDialer wires Dial to return a pre-built fakeBrewClient. Maps
// the target's host to its client so per-host scenarios are possible.
type fakeBrewDialer struct {
	clients  map[string]*fakeBrewClient
	dialErrs map[string]error
}

func (d *fakeBrewDialer) Dial(_ context.Context, t osh.Target, _ ...osh.Option) (RemoteClient, error) {
	if err := d.dialErrs[t.Host]; err != nil {
		return nil, err
	}
	c := d.clients[t.Host]
	if c == nil {
		return nil, errors.New("test: no fake client registered for " + t.Host)
	}
	return c, nil
}

func newFakeBrewClient() *fakeBrewClient {
	return &fakeBrewClient{
		stdoutByCmd: map[string]string{},
		stderrByCmd: map[string]string{},
		exitByCmd:   map[string]int{},
	}
}

func registerProbeFixture(c *fakeBrewClient, brewPath, brewVer, shellenv string, present, missing []string) {
	var sb strings.Builder
	sb.WriteString("arch=arm64\n")
	sb.WriteString("brew=" + brewPath + "\n")
	sb.WriteString("ver=" + brewVer + "\n")
	sb.WriteString("shellenv=" + shellenv + "\n")
	for _, p := range present {
		sb.WriteString("present=" + p + "\n")
	}
	for _, m := range missing {
		sb.WriteString("missing=" + m + "\n")
	}
	// The probe script contains "printf 'arch=" — use that as the match key.
	c.stdoutByCmd["printf 'arch="] = sb.String()
}

func TestBrewProbeOne_HealthyHost(t *testing.T) {
	c := newFakeBrewClient()
	registerProbeFixture(c, "/opt/homebrew/bin/brew", "Homebrew 4.1.0",
		"yes",
		[]string{"jq", "gh", "rg", "gtimeout", "shellcheck", "wget"},
		nil)
	d := &fakeBrewDialer{clients: map[string]*fakeBrewClient{"claw1.local": c}}
	target := brewTarget{Label: "hermes@claw1.local", Host: "claw1.local",
		SSH: osh.Target{User: "hermes", Host: "claw1.local"}}

	r := brewProbeOne(context.Background(), d, target, strings.Fields(defaultBrewEssentials), 5*time.Second)

	if !r.HasBrew() {
		t.Fatalf("HasBrew(): got false; r=%+v", r)
	}
	if r.BrewPath != "/opt/homebrew/bin/brew" {
		t.Errorf("BrewPath: got %q", r.BrewPath)
	}
	if r.BrewVer != "Homebrew 4.1.0" {
		t.Errorf("BrewVer: got %q", r.BrewVer)
	}
	if r.ShellenvIn != "yes" {
		t.Errorf("ShellenvIn: got %q want yes", r.ShellenvIn)
	}
	if len(r.Missing) != 0 {
		t.Errorf("Missing: got %v want none", r.Missing)
	}
	if len(r.Present) != 6 {
		t.Errorf("Present count: got %d want 6", len(r.Present))
	}
}

func TestBrewProbeOne_HostMissingBrew(t *testing.T) {
	c := newFakeBrewClient()
	registerProbeFixture(c, "MISSING", "", "no", nil,
		strings.Fields(defaultBrewEssentials))
	d := &fakeBrewDialer{clients: map[string]*fakeBrewClient{"claw9.local": c}}
	target := brewTarget{Label: "hermes@claw9.local", Host: "claw9.local",
		SSH: osh.Target{User: "hermes", Host: "claw9.local"}}

	r := brewProbeOne(context.Background(), d, target, strings.Fields(defaultBrewEssentials), 5*time.Second)

	if r.HasBrew() {
		t.Errorf("HasBrew(): got true for MISSING brew")
	}
	if r.BrewPath != "MISSING" {
		t.Errorf("BrewPath: got %q want MISSING", r.BrewPath)
	}
	if len(r.Missing) == 0 {
		t.Error("Missing should report all essentials when brew is absent")
	}
}

func TestBrewProbeOne_HostPartialEssentials(t *testing.T) {
	c := newFakeBrewClient()
	registerProbeFixture(c, "/opt/homebrew/bin/brew", "Homebrew 4.1.0",
		"yes",
		[]string{"jq", "gh"},
		[]string{"rg", "shellcheck", "wget", "gtimeout"})
	d := &fakeBrewDialer{clients: map[string]*fakeBrewClient{"claw2.local": c}}
	target := brewTarget{Label: "hermes@claw2.local", Host: "claw2.local",
		SSH: osh.Target{User: "hermes", Host: "claw2.local"}}

	r := brewProbeOne(context.Background(), d, target, strings.Fields(defaultBrewEssentials), 5*time.Second)

	if !r.HasBrew() {
		t.Fatal("HasBrew(): got false")
	}
	if len(r.Missing) != 4 {
		t.Errorf("Missing count: got %d (%v) want 4", len(r.Missing), r.Missing)
	}
	if len(r.Present) != 2 {
		t.Errorf("Present count: got %d (%v) want 2", len(r.Present), r.Present)
	}
}

func TestBrewProbeOne_DialError(t *testing.T) {
	d := &fakeBrewDialer{
		dialErrs: map[string]error{"claw3.local": errors.New("connection refused")},
	}
	target := brewTarget{Label: "hermes@claw3.local", Host: "claw3.local",
		SSH: osh.Target{User: "hermes", Host: "claw3.local"}}

	r := brewProbeOne(context.Background(), d, target, []string{"jq"}, 5*time.Second)

	if r.BrewPath != "probe-failed" {
		t.Errorf("BrewPath: got %q want probe-failed", r.BrewPath)
	}
	if !strings.Contains(r.Err, "connection refused") {
		t.Errorf("Err: got %q", r.Err)
	}
}

func TestParseBrewProbeOutput_HandlesBlankLinesAndNoise(t *testing.T) {
	out := `
arch=x86_64
brew=/usr/local/bin/brew
ver=Homebrew 4.0.0
shellenv=no

present=jq
missing=rg
missing=gh
unknown_key=ignored
`
	r := brewHostReport{}
	parseBrewProbeOutput(out, &r, []string{"jq", "rg", "gh"})

	if r.Arch != "x86_64" {
		t.Errorf("Arch: got %q", r.Arch)
	}
	if r.BrewPath != "/usr/local/bin/brew" {
		t.Errorf("BrewPath: got %q", r.BrewPath)
	}
	if r.ShellenvIn != "no" {
		t.Errorf("ShellenvIn: got %q", r.ShellenvIn)
	}
	if len(r.Missing) != 2 || r.Missing[0] != "gh" || r.Missing[1] != "rg" {
		t.Errorf("Missing: got %v want [gh rg] (sorted)", r.Missing)
	}
	if len(r.Present) != 1 || r.Present[0] != "jq" {
		t.Errorf("Present: got %v want [jq]", r.Present)
	}
}

func TestBrewEssentialsList_DefaultParse(t *testing.T) {
	brewEssentials = defaultBrewEssentials
	got := brewEssentialsList()
	want := []string{"jq", "coreutils", "shellcheck", "gh", "wget", "ripgrep"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d]: got %q want %q", i, got[i], w)
		}
	}
}

func TestBrewEssentialsList_StripsEmpties(t *testing.T) {
	brewEssentials = "  jq   gh "
	got := brewEssentialsList()
	if len(got) != 2 || got[0] != "jq" || got[1] != "gh" {
		t.Errorf("got %v want [jq gh]", got)
	}
}

func TestCountUnhealthy_MixedFleet(t *testing.T) {
	reports := []brewHostReport{
		{BrewPath: "/opt/homebrew/bin/brew"},                                 // ok
		{BrewPath: "MISSING"},                                                // unhealthy: no brew
		{BrewPath: "/opt/homebrew/bin/brew", Missing: []string{"jq"}},        // unhealthy: missing essentials
		{BrewPath: "probe-failed", Err: "timeout"},                           // unhealthy: probe failure
	}
	if got := countUnhealthy(reports); got != 3 {
		t.Errorf("got %d unhealthy want 3", got)
	}
}

func TestBrewMarkdownTable_IncludesRemediationWhenBrewMissing(t *testing.T) {
	reports := []brewHostReport{
		{Target: "hermes@claw1.local", Host: "claw1.local", Arch: "arm64",
			BrewPath: "/opt/homebrew/bin/brew", BrewVer: "4.1.0", ShellenvIn: "yes",
			Present: []string{"jq"}},
		{Target: "hermes@claw9.local", Host: "claw9.local", Arch: "arm64",
			BrewPath: "MISSING", ShellenvIn: "no", Missing: []string{"jq", "gh"}},
	}
	out := brewMarkdownTable(reports)
	for _, want := range []string{
		"| Host | Arch | Brew |",
		"hermes@claw1.local",
		"hermes@claw9.local",
		"MISSING",
		"Manual admin remediation needed",
		"Homebrew/install/HEAD/install.sh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nFull:\n%s", want, out)
		}
	}
}

func TestBrewMarkdownTable_NoRemediationWhenAllHealthy(t *testing.T) {
	reports := []brewHostReport{
		{Target: "hermes@claw1.local", Host: "claw1.local", Arch: "arm64",
			BrewPath: "/opt/homebrew/bin/brew", BrewVer: "4.1.0", ShellenvIn: "yes",
			Present: []string{"jq", "gh"}},
	}
	out := brewMarkdownTable(reports)
	if strings.Contains(out, "Manual admin remediation") {
		t.Errorf("remediation section leaked into healthy fleet output:\n%s", out)
	}
}

