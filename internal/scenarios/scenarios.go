// Package scenarios loads fleet test scenario YAML files and represents
// them as structured types for the `fleetctl test` runner.
//
// Schema:
//
//	name: string          — scenario identifier (used in --scenario filter)
//	tier: 1|2|3           — 1=smoke, 2=usecase, 3=e2e
//	description: string
//	vars:                 — optional key→value pairs, substituted as ${KEY} in step cmds
//	  NODE: hermes@claw1.local
//	steps:
//	  - name: string
//	    run: string       — shell command to execute (${VAR} expansion)
//	    expect:           — optional assertions
//	      exit: int       — assert exit code (default 0)
//	      stdout_contains: string
//	      stderr_contains: string
//	      stdout_absent: string
package scenarios

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tier classifies how expensive / destructive a scenario is.
type Tier int

const (
	TierSmoke   Tier = 1
	TierUsecase Tier = 2
	TierE2E     Tier = 3
)

func (t Tier) String() string {
	switch t {
	case TierSmoke:
		return "smoke"
	case TierUsecase:
		return "usecase"
	case TierE2E:
		return "e2e"
	default:
		return fmt.Sprintf("tier%d", int(t))
	}
}

// Expect holds optional assertions for a step's output.
type Expect struct {
	Exit           *int   `yaml:"exit"`
	StdoutContains string `yaml:"stdout_contains"`
	StderrContains string `yaml:"stderr_contains"`
	StdoutAbsent   string `yaml:"stdout_absent"`
}

// Step is one command in a scenario.
type Step struct {
	Name   string `yaml:"name"`
	Run    string `yaml:"run"`
	Expect Expect `yaml:"expect"`
}

// Scenario is the top-level struct parsed from a YAML file.
type Scenario struct {
	Name        string            `yaml:"name"`
	Tier        Tier              `yaml:"tier"`
	Description string            `yaml:"description"`
	Vars        map[string]string `yaml:"vars"`
	Steps       []Step            `yaml:"steps"`
}

// Validate checks that the scenario has the required fields.
func (s *Scenario) Validate() error {
	if s.Name == "" {
		return errors.New("scenarios: name is required")
	}
	if s.Tier < TierSmoke || s.Tier > TierE2E {
		return fmt.Errorf("scenarios: tier must be 1, 2, or 3; got %d", s.Tier)
	}
	for i, st := range s.Steps {
		if strings.TrimSpace(st.Run) == "" {
			return fmt.Errorf("scenarios: step[%d] %q has empty run", i, st.Name)
		}
	}
	return nil
}

// ExpandVars substitutes ${KEY} occurrences in cmd using s.Vars and any
// additional overrides supplied by the caller.
func (s *Scenario) ExpandVars(cmd string, overrides map[string]string) string {
	merged := make(map[string]string, len(s.Vars)+len(overrides))
	for k, v := range s.Vars {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	for k, v := range merged {
		cmd = strings.ReplaceAll(cmd, "${"+k+"}", v)
	}
	return cmd
}

// LoadFile parses a single scenario YAML file.
func LoadFile(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scenarios: read %s: %w", path, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("scenarios: parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// LoadDir loads all *.yaml files from dir, sorted by filename.
func LoadDir(dir string) ([]*Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("scenarios: read dir %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)

	var out []*Scenario
	for _, p := range paths {
		s, err := LoadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Filter returns only the scenarios whose name matches one of the names list
// (case-insensitive). If names is empty, all scenarios are returned.
func Filter(all []*Scenario, names []string, tier Tier) []*Scenario {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[strings.ToLower(n)] = true
	}
	var out []*Scenario
	for _, s := range all {
		if tier > 0 && s.Tier != tier {
			continue
		}
		if len(nameSet) > 0 && !nameSet[strings.ToLower(s.Name)] {
			continue
		}
		out = append(out, s)
	}
	return out
}
