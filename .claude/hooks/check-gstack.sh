#!/bin/bash
# PreToolUse hook: verify gstack skill bundle is installed before running any Skill.
# Owera Agentic requires gstack as the team toolset for all AI-assisted work.

set -euo pipefail

if [ -d "$HOME/.claude/skills/gstack/bin" ]; then
  exit 0
fi

cat >&2 <<'EOF'
[gstack-check] BLOCKED: gstack skill bundle not found at ~/.claude/skills/gstack/.

gstack is required for all AI-assisted work in this repo. Install:

  git clone --depth 1 https://github.com/garrytan/gstack.git ~/.claude/skills/gstack
  cd ~/.claude/skills/gstack && ./setup --team

Then restart Claude Code.
EOF

exit 1
