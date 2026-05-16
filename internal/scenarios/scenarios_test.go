package scenarios_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/owera/owera-fleet/internal/scenarios"
)

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(p, []byte(`
name: test-scenario
tier: 1
description: test
vars:
  NODE: hermes@claw1.local
steps:
  - name: echo
    run: echo ${NODE}
    expect:
      exit: 0
      stdout_contains: claw1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := scenarios.LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Name != "test-scenario" {
		t.Errorf("Name = %q, want test-scenario", s.Name)
	}
	if s.Tier != scenarios.TierSmoke {
		t.Errorf("Tier = %v, want smoke", s.Tier)
	}
	if len(s.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(s.Steps))
	}
	expanded := s.ExpandVars(s.Steps[0].Run, nil)
	if expanded != "echo hermes@claw1.local" {
		t.Errorf("ExpandVars = %q", expanded)
	}
}

func TestValidateMissingName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(p, []byte(`tier: 1\nsteps:\n  - name: x\n    run: echo hi\n`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := scenarios.LoadFile(p)
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.yaml", "a.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("name: "+name[:1]+"\ntier: 1\ndescription: x\nsteps:\n  - name: s\n    run: echo ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list, err := scenarios.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 scenarios, got %d", len(list))
	}
	// Should be sorted by filename: a.yaml before b.yaml.
	if list[0].Name != "a" {
		t.Errorf("first scenario = %q, want a", list[0].Name)
	}
}

func TestFilter(t *testing.T) {
	s1 := &scenarios.Scenario{Name: "s1", Tier: scenarios.TierSmoke}
	s2 := &scenarios.Scenario{Name: "s2", Tier: scenarios.TierUsecase}
	s3 := &scenarios.Scenario{Name: "s3", Tier: scenarios.TierSmoke}

	got := scenarios.Filter([]*scenarios.Scenario{s1, s2, s3}, nil, scenarios.TierSmoke)
	if len(got) != 2 {
		t.Errorf("tier filter: got %d, want 2", len(got))
	}
	got = scenarios.Filter([]*scenarios.Scenario{s1, s2, s3}, []string{"s2"}, 0)
	if len(got) != 1 || got[0].Name != "s2" {
		t.Errorf("name filter: got %v", got)
	}
}

func TestExpandVarsOverride(t *testing.T) {
	s := &scenarios.Scenario{
		Vars: map[string]string{"A": "default"},
	}
	cmd := s.ExpandVars("echo ${A} ${B}", map[string]string{"B": "extra"})
	if cmd != "echo default extra" {
		t.Errorf("ExpandVars = %q", cmd)
	}
}
