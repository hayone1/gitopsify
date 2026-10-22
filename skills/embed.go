// Package skills holds the agent skills for AI coding assistants that
// gitopsify installs with "gitopsify skills install". Each directory is one
// skill in the Agent Skills format (a SKILL.md with name and description,
// plus optional references), read by Claude Code, Codex, Gemini CLI, Cursor,
// GitHub Copilot and other agents.
package skills

import "embed"

// FS holds every skill directory.
//
//go:embed */SKILL.md */references/*.md
var FS embed.FS
