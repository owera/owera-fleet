package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderSkillDeterministic(t *testing.T) {
	s := Skill{
		Name:    "state",
		Trigger: "/state",
		Summary: "Regenerate snapshot.",
		Args: []SkillArg{
			{Name: "--json", Description: "Emit JSON."},
		},
		Examples: []SkillExample{
			{Description: "Default run", Command: "fleetctl state"},
		},
	}
	a := RenderSkill(s)
	b := RenderSkill(s)
	if !bytes.Equal(a, b) {
		t.Fatalf("RenderSkill is non-deterministic:\nA:\n%s\nB:\n%s", a, b)
	}
	str := string(a)
	for _, want := range []string{
		"name: owera-fleet/state",
		"# state",
		"**Trigger:** `/state`",
		"## Summary",
		"Regenerate snapshot.",
		"## Arguments",
		"| `--json` | Emit JSON. |",
		"## Examples",
		"**Default run:**",
		"fleetctl state",
	} {
		if !strings.Contains(str, want) {
			t.Errorf("missing %q in:\n%s", want, str)
		}
	}
}

func TestRenderSkillEscapesTablePipe(t *testing.T) {
	s := Skill{Name: "x", Args: []SkillArg{{Name: "--y", Description: "a|b"}}}
	out := string(RenderSkill(s))
	if !strings.Contains(out, `a\|b`) {
		t.Errorf("expected escaped pipe, got:\n%s", out)
	}
}

func TestGenSkillsWritesAndChecks(t *testing.T) {
	dir := t.TempDir()

	// Override the global flag so we don't trample a real ./skills.
	prev := genSkillsDir
	genSkillsDir = dir
	t.Cleanup(func() { genSkillsDir = prev })

	// Sanity: registry has at least state + gen-skills.
	if got := len(AllSkills()); got < 2 {
		t.Fatalf("expected ≥2 registered skills, got %d", got)
	}

	// Write phase.
	if err := runGenSkills(genSkillsCmd, nil); err != nil {
		t.Fatalf("write phase: %v", err)
	}
	for _, s := range AllSkills() {
		path := filepath.Join(dir, s.Name, "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s on disk: %v", path, err)
		}
	}

	// Check phase: should report clean.
	genSkillsCheck = true
	t.Cleanup(func() { genSkillsCheck = false })
	if err := runGenSkills(genSkillsCmd, nil); err != nil {
		t.Fatalf("check after fresh write should be clean: %v", err)
	}

	// Mutate one file → check must fail.
	skills := AllSkills()
	path := filepath.Join(dir, skills[0].Name, "SKILL.md")
	if err := os.WriteFile(path, []byte("tampered\n"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := runGenSkills(genSkillsCmd, nil)
	if err == nil {
		t.Fatalf("expected drift error, got nil")
	}
	if !strings.Contains(err.Error(), "out of sync") {
		t.Errorf("expected drift message, got %v", err)
	}
}

func TestGenSkillsCheckMissingFile(t *testing.T) {
	dir := t.TempDir()
	prev := genSkillsDir
	genSkillsDir = dir
	prevCheck := genSkillsCheck
	genSkillsCheck = true
	t.Cleanup(func() {
		genSkillsDir = prev
		genSkillsCheck = prevCheck
	})
	err := runGenSkills(genSkillsCmd, nil)
	if err == nil {
		t.Fatalf("expected missing-file drift error, got nil")
	}
}
