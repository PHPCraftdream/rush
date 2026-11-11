# Delegation command installation and refresh

Each `*-init` command renders the embedded Rush delegation sources from the **current binary**. Run it after delivering the matching binary when updating an installation. Choose each CLI and scope explicitly; this is not a broad-discovery or automatic update mechanism.

## Supported outputs

All init commands are global by default. `--local` or `--cwd <project>` selects the named project's paths; `--cwd` implies local scope. `--global` explicitly selects global scope. Global paths below are relative to the user's home directory.

| CLI | Init command | Global artifacts | Project artifacts |
| --- | --- | --- | --- |
| Claude Code | `claude-init` | `.claude/commands/{rush,rush-fallback,wrush,wcrush}.md` | `<cwd>/.claude/commands/{rush,rush-fallback,wrush,wcrush}.md` |
| Codex CLI | `codex-init` | `.agents/skills/{rush,rush-fallback,wrush,wcrush}/SKILL.md` | `<cwd>/.agents/skills/{rush,rush-fallback,wrush,wcrush}/SKILL.md` |
| OpenCode | `opencode-init` | `.config/opencode/commands/{rush,rush-fallback,wrush,wcrush}.md` | `<cwd>/.opencode/commands/{rush,rush-fallback,wrush,wcrush}.md` |
| Oh My Pi | `omp-init` | `.omp/agent/commands/{rush,rush-fallback,wrush,wcrush}.md` | `<cwd>/.omp/commands/{rush,rush-fallback,wrush,wcrush}.md` |
| Gemini CLI | `gemini-init` | `.gemini/commands/{rush,rush-fallback}.toml` | `<cwd>/.gemini/commands/{rush,rush-fallback}.toml` |
| Grok Build CLI | `grok-init` | `.grok/skills/{rush,rush-fallback}/SKILL.md` | `<cwd>/.grok/skills/{rush,rush-fallback}/SKILL.md` |
| Qwen Code CLI | `qwen-init` | `.qwen/commands/{rush,rush-fallback}.md` | `<cwd>/.qwen/commands/{rush,rush-fallback}.md` |

The shared `/rush` and `/rush-fallback` source is `internal/cmd/claude_slash_command.yaml` and `internal/cmd/claude_crush_fallback_command.yaml`; `/wrush` and `/wcrush` use `internal/cmd/claude_wrush_command.md` and `internal/cmd/claude_wcrush_command.md`. `internal/cmd/multi_cli_convert.go` assembles target-specific output; each integration's `*-init` specifies which of those four it installs (Claude/Codex/OpenCode/OMP install all four; Gemini/Grok/Qwen install the first two). External per-model commands, agents, or other artifacts are **not** managed by these Rush installers.

## Ownership and safe refresh boundaries

Installers test for the Rush sentinel substring in each expected file. When the sentinel is present, init replaces that file's entire contents with the version rendered by the current binary; local edits are not merged. When the target name exists without the sentinel, init skips it and prints a warning. That collision can still leave the command with exit status 0, so success alone does not mean every target was refreshed. A missing target is created.

For a controlled update, deliver the binary first, then invoke only the relevant `rush <tool>-init` command(s), using the explicitly intended global or project scope. The installer itself enforces per-file sentinel checks; do not treat the existence of a tool directory or a same-name file as proof that all artifacts are Rush-owned. Avoid broad recursive/global refreshes when the requirement is to update only known Rush-owned artifacts.

`cli-refresh` is not a complete substitute: it maps only Claude, Codex, Gemini, Grok, and Qwen, not OpenCode or Oh My Pi. Its Claude helper refreshes/removes only `rush.md` and `rush-fallback.md`, not `wrush.md` or `wcrush.md`. The supported direct `*-init` commands are the way to refresh the intended complete set.

**Claude project-scope side effect:** `claude-init --local` also strips any legacy `crush-claude-init` block from `<cwd>/CLAUDE.md`; it deletes that file if stripping leaves it empty. Do not run local Claude init if the update must leave `CLAUDE.md` untouched. Global Claude init does not perform this project-local cleanup.

## Source references

- `internal/cmd/claude_init.go`: Claude defaults to `~/.claude/commands/`; local init writes four command files and performs the legacy local `CLAUDE.md` cleanup.
- `internal/cmd/codex_init.go`, `opencode_init.go`, `omp_init.go`, `gemini_init.go`, `grok_init.go`, `qwen_init.go`: remaining path mappings and installed command sets.
- `internal/cmd/multi_cli_convert.go`: source parsing, target conversion, and sentinel-aware file writes; `writeSentinelledFile` overwrites only when the sentinel substring is found and otherwise warns and skips.
- `internal/cmd/cli_refresh.go`: five-tool refresh mapping and the two-file-only Claude helper.
