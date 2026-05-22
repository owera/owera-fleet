package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	osh "github.com/owera/owera-fleet/internal/ssh"
)

// registerScrSharingProbeFixture writes the canned probe output the
// scrSharingProbeOne caller expects, keyed off the probe script's
// distinctive `printf 'listening=%s\n'` line. Matches the matchKey
// helper from brew_test.go.
func registerScrSharingProbeFixture(c *fakeBrewClient, listening bool) {
	v := "no"
	if listening {
		v = "yes"
	}
	// printf 'listening=%s\n' "$listening"  →  output:
	c.stdoutByCmd["printf 'listening="] = "listening=" + v + "\n"
}

func TestScrSharingProbeOne_Listening(t *testing.T) {
	c := newFakeBrewClient()
	registerScrSharingProbeFixture(c, true)
	d := &fakeBrewDialer{clients: map[string]*fakeBrewClient{"claw1.local": c}}
	target := brewTarget{Label: "hermes@claw1.local", Host: "claw1.local",
		SSH: osh.Target{User: "hermes", Host: "claw1.local"}}

	s := scrSharingProbeOne(context.Background(), d, target, 5*time.Second)
	if s.HasError() {
		t.Fatalf("unexpected probe error: %s", s.Err)
	}
	if !s.Listening {
		t.Errorf("Listening: got false, want true")
	}
}

func TestScrSharingProbeOne_NotListening(t *testing.T) {
	c := newFakeBrewClient()
	registerScrSharingProbeFixture(c, false)
	d := &fakeBrewDialer{clients: map[string]*fakeBrewClient{"claw2.local": c}}
	target := brewTarget{Label: "hermes@claw2.local", Host: "claw2.local",
		SSH: osh.Target{User: "hermes", Host: "claw2.local"}}

	s := scrSharingProbeOne(context.Background(), d, target, 5*time.Second)
	if s.HasError() {
		t.Fatalf("unexpected probe error: %s", s.Err)
	}
	if s.Listening {
		t.Errorf("Listening: got true, want false")
	}
}

func TestScrSharingProbeOne_DialError(t *testing.T) {
	d := &fakeBrewDialer{
		dialErrs: map[string]error{"claw3.local": errors.New("connection refused")},
	}
	target := brewTarget{Label: "hermes@claw3.local", Host: "claw3.local",
		SSH: osh.Target{User: "hermes", Host: "claw3.local"}}

	s := scrSharingProbeOne(context.Background(), d, target, 5*time.Second)
	if !s.HasError() {
		t.Fatal("expected probe error")
	}
	if !strings.Contains(s.Err, "connection refused") {
		t.Errorf("Err: got %q", s.Err)
	}
}

func TestParseScrSharingProbe_HandlesNoiseAndBlanks(t *testing.T) {
	out := `
listening=yes

extra=ignored
`
	s := scrSharingState{}
	parseScrSharingProbe(out, &s)
	if !s.Listening {
		t.Errorf("Listening: got false, want true")
	}
}

func TestScrSharingClassifyError_SudoRequired(t *testing.T) {
	cases := []string{
		"sudo: a password is required",
		"sudo: a terminal is required to read the password",
		"some other prefix then a password is required",
	}
	for _, in := range cases {
		got := scrSharingClassifyError(in, errors.New("remote exit 1"))
		if got != "sudo-required" {
			t.Errorf("classify(%q): got %q want sudo-required", in, got)
		}
	}
}

func TestScrSharingClassifyError_PlistMissing(t *testing.T) {
	out := "/bin/launchctl: No such file or directory"
	got := scrSharingClassifyError(out, errors.New("remote exit 1"))
	if got != "plist-missing" {
		t.Errorf("classify: got %q want plist-missing", got)
	}
}

func TestScrSharingClassifyError_Other(t *testing.T) {
	out := "launchctl: load failed: 5: Input/output error"
	got := scrSharingClassifyError(out, errors.New("remote exit 5"))
	if got != "error" {
		t.Errorf("classify: got %q want error", got)
	}
}

func TestScrSharingSudoersStanza_WellFormed(t *testing.T) {
	got := scrSharingSudoersStanza()
	for _, want := range []string{
		"NOPASSWD",
		"/bin/launchctl load -w " + screenSharingPlist,
		"/bin/launchctl unload -w " + screenSharingPlist,
		sudoersStanzaUser + " ALL=(root)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stanza missing %q. full output:\n%s", want, got)
		}
	}
	// Sudoers files are line-oriented and the rule should sit on one line.
	// Splitting on "\n" should give us at least one non-comment line that
	// starts with the user name.
	hasRuleLine := false
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, sudoersStanzaUser+" ") {
			hasRuleLine = true
			break
		}
	}
	if !hasRuleLine {
		t.Errorf("stanza has no rule line starting with %q", sudoersStanzaUser+" ")
	}
}

func TestScrSharingProbeResultLabel(t *testing.T) {
	cases := []struct {
		s    scrSharingState
		want string
	}{
		{scrSharingState{Listening: true}, "on"},
		{scrSharingState{Listening: false}, "off"},
		{scrSharingState{Err: "dial: refused"}, "error"},
	}
	for _, c := range cases {
		got := scrSharingProbeResultLabel(c.s)
		if got != c.want {
			t.Errorf("label(%+v): got %q want %q", c.s, got, c.want)
		}
	}
}

func TestScrSharingMarkdownTable_SudoRemediationFootnote(t *testing.T) {
	reports := []scrSharingState{
		{Target: "hermes@claw1.local", Host: "claw1.local", Listening: false, Result: "sudo-required", StderrTail: "sudo: a password is required"},
	}
	out := scrSharingMarkdownTable(reports, "enable")
	if !strings.Contains(out, "Sudo required") {
		t.Errorf("expected sudo remediation footnote; got:\n%s", out)
	}
	if !strings.Contains(out, "setup-sudoers") {
		t.Errorf("expected setup-sudoers pointer; got:\n%s", out)
	}
}

func TestScrSharingMarkdownTable_NoFootnoteWhenNoSudoErrors(t *testing.T) {
	reports := []scrSharingState{
		{Target: "hermes@claw1.local", Host: "claw1.local", Listening: true, Result: "ok"},
	}
	out := scrSharingMarkdownTable(reports, "enable")
	if strings.Contains(out, "Sudo required") {
		t.Errorf("unexpected sudo footnote in clean run; got:\n%s", out)
	}
}

func TestScrSharingJSONReport_ShapesMatch(t *testing.T) {
	reports := []scrSharingState{
		{Target: "gateway", Host: "claw3", Listening: true, Result: "on"},
		{Target: "hermes@claw1.local", Host: "claw1.local", Listening: false, Result: "off"},
	}
	out := scrSharingJSONReport(reports)
	for _, want := range []string{
		`"target":"gateway"`,
		`"target":"hermes@claw1.local"`,
		`"listening":true`,
		`"listening":false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %q. full output:\n%s", want, out)
		}
	}
	// One line per host.
	lines := 0
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("expected 2 JSON lines, got %d", lines)
	}
}

func TestScrSharingToggleScript_VerbAndPlist(t *testing.T) {
	enable := scrSharingToggleScript(true)
	disable := scrSharingToggleScript(false)
	if !strings.Contains(enable, "sudo -n /bin/launchctl load -w "+screenSharingPlist) {
		t.Errorf("enable script: got %q", enable)
	}
	if !strings.Contains(disable, "sudo -n /bin/launchctl unload -w "+screenSharingPlist) {
		t.Errorf("disable script: got %q", disable)
	}
}
