// skills.go declares the metadata schema fleetctl gen-skills uses to
// render skills/<name>/SKILL.md files. Each subcommand registers its
// Skill() function with registerSkill in its init(); gen-skills walks the
// registry, ensures it matches cobra's registered subcommands, and emits
// the manifest files.
//
// Keeping the registry here (rather than per-command) means commands can
// declare metadata without importing a third-party skill package, and the
// gen-skills command in turn does not need to reach into private state on
// each cobra command.
package commands

import "sort"

// Skill is the metadata that becomes one SKILL.md file.
type Skill struct {
	Name     string
	Trigger  string
	Summary  string
	Args     []SkillArg
	Examples []SkillExample
}

// SkillArg documents a single flag or positional argument.
type SkillArg struct {
	Name        string
	Description string
}

// SkillExample is one "task: command" pair under the Examples section.
type SkillExample struct {
	Description string
	Command     string
}

// skillRegistry is keyed by command name (e.g. "state"). The value is the
// function returning the skill metadata; lazy evaluation lets each command
// register itself in init() without ordering dependencies between files.
var skillRegistry = map[string]func() Skill{}

// registerSkill records fn under name. Called from each command file's
// init(). Panics on duplicate registration so a copy-paste mistake fails
// loudly at startup rather than silently shadowing a real command.
func registerSkill(name string, fn func() Skill) {
	if _, dup := skillRegistry[name]; dup {
		panic("commands: duplicate skill registration for " + name)
	}
	skillRegistry[name] = fn
}

// AllSkills returns every registered skill, sorted by Name. Exported for
// use by gen_skills.go (T1.6) and by tests.
func AllSkills() []Skill {
	out := make([]Skill, 0, len(skillRegistry))
	for _, fn := range skillRegistry {
		out = append(out, fn())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
