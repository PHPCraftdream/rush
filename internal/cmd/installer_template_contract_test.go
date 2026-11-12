package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstalledRushTemplatesPreserveLivenessAndCallerPolicyContract(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(temp, "global-data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(temp, "global-config"))
	t.Setenv("HOME", filepath.Join(temp, "home"))
	t.Setenv("USERPROFILE", filepath.Join(temp, "userprofile"))

	type installCase struct {
		name            string
		taskPlaceholder string
		body            func(t *testing.T, root string) string
	}
	cases := []installCase{
		{name: "claude", taskPlaceholder: "$ARGUMENTS", body: func(t *testing.T, root string) string {
			dir := filepath.Join(root, ".claude", "commands")
			require.NoError(t, installClaudeCommands(dir))
			require.NoError(t, writeWrushCommandToDir(dir))
			require.NoError(t, writeWcrushCommandToDir(dir))
			wrush := readTemplateFixture(t, filepath.Join(dir, "wrush.md"))
			wcrush := readTemplateFixture(t, filepath.Join(dir, "wcrush.md"))
			require.Contains(t, wrush, "absolute paths, subprocesses, other tools, or external workers may still access outside the worktree")
			require.Contains(t, wcrush, "`wrush.md` file in this same directory")
			return readTemplateFixture(t, filepath.Join(dir, "rush.md"))
		}},
		{name: "codex", taskPlaceholder: "$ARGUMENTS", body: func(t *testing.T, root string) string {
			require.NoError(t, installCodexSkills(root))
			for _, name := range []string{"rush", "wrush", "wcrush"} {
				body := readTemplateFixture(t, filepath.Join(root, name, "SKILL.md"))
				if name == "rush" {
					require.Contains(t, body, "CI-only verification is requested")
					require.Contains(t, body, "A held child question remains answerable via `inject_agent`")
				}
				if name == "wrush" {
					require.Contains(t, body, "sibling `../rush/SKILL.md` file")
					require.Contains(t, body, "Worktree-scoped `rush run` is delegation layer one")
				}
				if name == "wcrush" {
					require.Contains(t, body, "Phase-2 verification follows caller policy")
					require.Contains(t, body, "sibling `../wrush/SKILL.md` file")
					require.Contains(t, body, "../wrush/SKILL.md's checklist")
				}
			}
			return readTemplateFixture(t, filepath.Join(root, "rush", "SKILL.md"))
		}},
		{name: "gemini", taskPlaceholder: "{{args}}", body: func(t *testing.T, root string) string {
			require.NoError(t, installGeminiCommands(root))
			body := readTemplateFixture(t, filepath.Join(root, "rush.toml"))
			require.Contains(t, body, "CI-only verification is requested")
			return body
		}},
		{name: "grok", taskPlaceholder: "$ARGUMENTS", body: func(t *testing.T, root string) string {
			require.NoError(t, installGrokSkills(root))
			body := readTemplateFixture(t, filepath.Join(root, "rush", "SKILL.md"))
			require.Contains(t, body, "CI-only verification is requested")
			return body
		}},
		{name: "qwen", taskPlaceholder: "{{args}}", body: func(t *testing.T, root string) string {
			require.NoError(t, installQwenCommands(root))
			body := readTemplateFixture(t, filepath.Join(root, "rush.md"))
			require.Contains(t, body, "CI-only verification is requested")
			return body
		}},
		{name: "opencode", taskPlaceholder: "$ARGUMENTS", body: func(t *testing.T, root string) string {
			require.NoError(t, installMarkdownCommands(root, skillTargetOpenCode))
			body := readTemplateFixture(t, filepath.Join(root, "wcrush.md"))
			require.Contains(t, body, "Phase-2 verification follows caller policy")
			return readTemplateFixture(t, filepath.Join(root, "rush.md"))
		}},
		{name: "omp", taskPlaceholder: "$ARGUMENTS", body: func(t *testing.T, root string) string {
			require.NoError(t, installMarkdownCommands(root, skillTargetOmp))
			body := readTemplateFixture(t, filepath.Join(root, "wcrush.md"))
			require.Contains(t, body, "Phase-2 verification follows caller policy")
			return readTemplateFixture(t, filepath.Join(root, "rush.md"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t, filepath.Join(temp, tc.name))
			for _, contract := range []string{
				"retain the handle and wait for that evidence",
				"never infer physical process exit from `sessions why` saying `crashed`",
				"A top-level run can exit while other independent batch roots remain",
				"relevant owned/related batch work is terminal",
				"A held child question remains answerable via `inject_agent`",
				"`end_turn` ends an assistant turn, not necessarily the task",
				"`between_turns` means a run loop is still waiting/continuing",
				"`unknown` means the read did not establish state",
				"Output truncation and `sessions truncate`/`reset` are not context compaction",
				"CI-only verification is requested",
				"Otherwise run only checks the caller authorized",
				"original supported role (`--role` is required)",
				"agent(resume_session_id=",
				"inspect_agent` reports `awaiting_answer` and `held_question` identity",
				"inject_agent(child_session_id=",
				"default six-hour (6 h) TOTAL wall-clock cap",
				"including active work and all waits",
				"progress does not reset this cap",
				"`RUSH_RUN_DEFAULT_HARD_TIMEOUT` overrides the default cap only",
				"A positive `--timeout` replaces the default cap",
				"`--timeout-extends-on-progress` extends only the per-turn stream watchdog, not the total deadline",
				"If the process remains alive 60 seconds later, the hard backstop force-kills it (exit 124)",
				"`--idle-timeout` (default `15m`)",
				"It watches turns only",
				"do not automatically raise `--timeout` or infer deadlock or its absence from a timeout",
				"`rush sessions compact <session> [--role worker|smart|fast|reviewer]` defaults to `worker`",
				"uses ordinary history summarization (`SummarizeNow`), not `Run`, a new leaf, replay, or a queue",
				"Role resolution is the session override then the matching configured slot; unavailable roles, including custom roles, error without fallback",
				"This is direct-provider-only; CLI backends are refused",
				"Combined live or unreadable activity refuses immediately under mailbox/OS-lock coordination; busy work is not killed, bypassed, queued, or automatically resumed",
				"A summary may exceed worker capacity; errors preserve original history, without a universal recovery guarantee",
				"an exact structured `context_length_exceeded` error certified before any output or usage (HTTP 400 or SSE `response.failed` or strictly guarded nested/flat `type=error` envelopes)",
				"one silent-prefix summary and continuation in that same session",
				"Partial, opaque, or uncertified output, transport errors, and NoReplay-fenced failures are never replayed",
				"captured matching worker session override/config (or covered per-call credentials)",
				"there is no silent role/provider fallback, and CLI workers are unsupported",
				"without re-executing accepted tools",
				"Missing/unavailable workers, unknown or insufficient capacity, budget caps, or failed/cancelled summaries fail truthfully",
				"history is not deleted before successful publication",
				"There is one recovery attempt and continuation; a second overflow is terminal",
				"no task size, including task 371, is guaranteed to fit",
				"This does not resurrect an already-exited process; an ordinary run/resume requires the matching installed release",
				"Ended external processes are not resurrected; no consumer-session mutation occurs",
				"With no session, `rush sessions compact` remains database-only SQLite `VACUUM`",
				"`--dry-run`, `--force`, and `--json` are DB-only and rejected with a session even when false; `--role` without a session is rejected",
				"Database `--dry-run` may checkpoint WAL",
				"Output truncation and `sessions truncate`/`reset` are not context compaction",
				"## Time limits",
				"## Task",
			} {
				require.Contains(t, oneLine(body), contract)
			}
			taskSlot := "## Task\n\n" + tc.taskPlaceholder
			require.Equal(t, 1, strings.Count(body, taskSlot))
			if tc.taskPlaceholder == "{{args}}" {
				require.Equal(t, 1, strings.Count(body, "{{args}}"))
				require.NotContains(t, body, "$ARGUMENTS")
			} else {
				require.NotContains(t, body, "{{args}}")
			}
			require.Equal(t, 1, strings.Count(body, "## Time limits"))
			require.Equal(t, 1, strings.Count(body, "## Task"))
			require.NotContains(t, body, "`crashed` means process gone")
			require.NotContains(t, body, "Confirm process termination with `why` or `show")
		})
	}
}

func TestInstalledFallbackGuidanceDistinguishesContextCapacityFromQuota(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(temp, "global-data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(temp, "global-config"))
	t.Setenv("HOME", filepath.Join(temp, "home"))
	t.Setenv("USERPROFILE", filepath.Join(temp, "userprofile"))

	cases := []struct {
		name    string
		path    string
		install func(root string) error
	}{
		{name: "claude", path: filepath.Join(".claude", "commands", "rush-fallback.md"), install: func(root string) error {
			return installClaudeCommands(filepath.Join(root, ".claude", "commands"))
		}},
		{name: "codex", path: filepath.Join("rush-fallback", "SKILL.md"), install: installCodexSkills},
		{name: "gemini", path: "rush-fallback.toml", install: installGeminiCommands},
		{name: "grok", path: filepath.Join("rush-fallback", "SKILL.md"), install: installGrokSkills},
		{name: "qwen", path: "rush-fallback.md", install: installQwenCommands},
		{name: "opencode", path: "rush-fallback.md", install: func(root string) error {
			return installMarkdownCommands(root, skillTargetOpenCode)
		}},
		{name: "omp", path: "rush-fallback.md", install: func(root string) error {
			return installMarkdownCommands(root, skillTargetOmp)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(temp, tc.name)
			require.NoError(t, tc.install(root))
			fallback := readTemplateFixture(t, filepath.Join(root, tc.path))
			require.Contains(t, fallback, "A context-capacity overflow is NOT account quota exhaustion and does not arm or activate fallback.")
			require.Contains(t, fallback, "Follow `/rush`'s bounded context recovery or its reported refusal; do not route around terminal recovery.")
			require.NotContains(t, fallback, `"context window exceeded"`)
			require.NotContains(t, fallback, `"quota exceeded"`)

			if tc.name == "claude" {
				require.Contains(t, fallback, "provider explicitly reports account quota exhaustion")
				require.Contains(t, fallback, "is in peak hours (")
				require.Contains(t, fallback, "rush-fallback state (persistent — do not complete)")
				require.Contains(t, fallback, "CronCreate")
				require.Contains(t, fallback, "Only `/rush-fallback clear` ends it.")
			}
		})
	}
}

func TestInstalledWrushAndWcrushRespectParentOwnershipAndCIOnly(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(temp, "global-data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(temp, "global-config"))
	t.Setenv("HOME", filepath.Join(temp, "home"))
	t.Setenv("USERPROFILE", filepath.Join(temp, "userprofile"))

	claudeDir := filepath.Join(temp, "claude", ".claude", "commands")
	require.NoError(t, writeWrushCommandToDir(claudeDir))
	require.NoError(t, writeWcrushCommandToDir(claudeDir))
	wrush := readTemplateFixture(t, filepath.Join(claudeDir, "wrush.md"))
	wcrush := readTemplateFixture(t, filepath.Join(claudeDir, "wcrush.md"))

	require.Contains(t, wrush, "Only after the `rush run` process has physically exited")
	require.Contains(t, wrush, "task's relevant owned/related batch work is terminal")
	require.Contains(t, wrush, "The invoking parent owns any commit and worktree/branch cleanup")
	require.Contains(t, wrush, "Commit the transferred changes in the primary branch only after reviewing the staged diff")
	require.Contains(t, wrush, "After the merge is committed, remove the worktree")
	require.Contains(t, wrush, "Worktree transfer, commit, and removal are not automatic cleanup requirements")
	require.Contains(t, wcrush, "do not commit or remove the worktree unconditionally")
	require.Contains(t, wcrush, "`/wrush`'s dedicated git worktree and no-sub-agent-commit/push rule remain in force")
	require.Contains(t, wcrush, "The invoking parent owns transfer, commit, and cleanup")
	require.Contains(t, wcrush, "authorized and after physical process plus relevant batch termination")
	require.Contains(t, wcrush, "When authorized after these gates, the parent commits only those changes before removing the worktree")
	require.Contains(t, wcrush, "when CI-only execution is requested")
	require.Contains(t, wcrush, "wrush.md's checklist")
	require.Contains(t, wcrush, "parent commits only those changes before removing the worktree")
	require.NotContains(t, wcrush, "MUST run these memory-light checks")
	require.NotContains(t, wcrush, "go build ./...")
	require.NotContains(t, wcrush, "go vet ./...")
}

func readTemplateFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}
