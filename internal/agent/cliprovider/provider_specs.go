// The CLI model catalog: the CLISpec type, per-CLI argument builders,
// spec constructors, the codex MCP -c override args, and the [All] list
// registering every hardcoded CLI-backed model.

package cliprovider

import (
	"fmt"

	"charm.land/fantasy"
)

// CLISpec describes how to invoke a single CLI-based language model.
type CLISpec struct {
	// ModelID is the model identifier used in the crush UI (e.g. "cli-claude").
	ModelID string
	// ModelName is the human-readable display name.
	ModelName string
	// ContextWindow is the total context window size in tokens.
	// Used by crush to decide when to trigger auto-summarization.
	ContextWindow int64
	// Binary is the executable name resolved via PATH (e.g. "claude", "gemini").
	Binary string
	// PromptFlag is the CLI flag used to pass the prompt inline (e.g. "-p").
	// When the prompt exceeds maxPromptArgLen, it is piped via stdin instead.
	PromptFlag string
	// BuildArgs returns the CLI arguments for a given yolo flag.
	// The prompt is NOT included — it is added separately by Stream.
	BuildArgs func(yolo bool) []string
	// NewPartParser returns a stateful function that maps a JSON line to a
	// StreamPart. Supports text and reasoning (thinking) deltas. If nil, raw
	// text mode is used (lines are stripped of ANSI escapes and yielded as-is).
	NewPartParser func() func(line []byte) (fantasy.StreamPart, bool)
	// ParseUsageLine parses token usage from a single output line.
	// Called on every line; returns (usage, true) when usage data is found.
	// If nil, usage will be zero in the Finish stream part.
	ParseUsageLine func(line []byte) (fantasy.Usage, bool)
	// UseCrushMCP controls whether crush starts an internal MCP server and
	// passes it to the CLI process via --mcp-config.  When true and the
	// provider is running in non-yolo mode, tool calls are routed through
	// crush's permission system instead of the CLI's own permission handling.
	UseCrushMCP bool
	// AlwaysStdin forces the prompt to be delivered via stdin instead of a
	// CLI flag, and disables PTY mode (using a regular pipe instead).
	// Use this for CLIs that detect TTY on stdout and switch to interactive
	// mode rather than emitting JSON, even when --output-format stream-json
	// is specified.
	AlwaysStdin bool
	// NoPTY skips PTY mode and always uses pipe-based I/O, while still
	// passing the prompt via PromptFlag (unlike AlwaysStdin which also
	// forces stdin delivery). Use this for wrapper binaries like npx.cmd
	// that don't relay child-process output through ConPTY on Windows.
	NoPTY bool
	// QwenMCPIntegration starts crush's MCP server and registers it in
	// ~/.qwen/settings.json under a stable per-project ID stored in
	// <workingDir>/.crush/qwen-mcp-id. The entry is removed when the CLI
	// process exits. Uses Authorization: Bearer header for auth.
	QwenMCPIntegration bool
	// GeminiMCPIntegration starts crush's MCP server and registers it in
	// ~/.gemini/settings.json under a stable per-project ID stored in
	// <workingDir>/.crush/gemini-mcp-id. The entry is removed when the CLI
	// process exits. Uses Authorization: Bearer header and trust:true to
	// bypass Gemini's own confirmation prompts.
	GeminiMCPIntegration bool
	// CodexMCPIntegration starts crush's MCP server and passes its URL to
	// codex via -c flags (inline config override), so no persistent changes
	// are made to ~/.codex/config.toml. The token is passed via the
	// CRUSH_CODEX_MCP_TOKEN environment variable, which codex reads via
	// bearer_token_env_var and sends as Authorization: Bearer header.
	CodexMCPIntegration bool
	// SupportsResume enables --resume <session_id> for CLI models that
	// support it (Claude CLI). This lets the CLI reload its own conversation
	// history from its local DB, enabling API-level prompt caching across
	// multiple messages in the same crush session.
	SupportsResume bool
	// ApplyEffort adds a reasoning-effort setting to the CLI arguments.
	//
	// nil means this CLI has NO effort option, and the session's stored effort
	// must be silently dropped rather than passed along: gemini and qwen abort
	// with "Unknown argument: effort" and codex with "unexpected argument
	// '--effort'". Only claude takes a --effort flag; codex takes the same
	// idea as `-c model_reasoning_effort=<level>`. See effort.go.
	ApplyEffort func(args []string, effort string) []string
	// EffortLevels lists the values THIS model accepts. Empty means "not
	// validated". Levels are per-model, not per-CLI — codex's gpt-5.6-sol
	// accepts "ultra" while gpt-5.5 stops at "xhigh" — so a stale effort that
	// is legal on another model is dropped here instead of being forwarded
	// into a 400 from the provider.
	EffortLevels []string
}

// claudeArgs returns a BuildArgs func for a claude CLI model.
// extra allows passing additional static flags (e.g. "--effort", "high").
func claudeArgs(model string, extra ...string) func(bool) []string {
	return func(yolo bool) []string {
		args := []string{
			"--model", model,
			"--output-format", "stream-json",
			"--verbose",
			"--include-partial-messages",
		}
		args = append(args, extra...)
		if yolo {
			args = append(args, "--dangerously-skip-permissions")
		}
		return args
	}
}

// npxClaudeArgs was used for the cli-npx-claude-* family. Removed in the
// 2026-06-17 cleanup — the variants doubled the model list with no real
// benefit (anyone with `claude` on PATH gets identical behaviour faster,
// and Windows ConPTY relay through npx.cmd was unreliable anyway).

// codexMCPTokenEnvVar is the name of the environment variable crush sets on
// the codex child process to carry the MCP auth token (P2-3). Passing the
// token via an env var + Authorization: Bearer header, instead of embedding
// it in the -c inline config override's URL as a query parameter, avoids
// exposing it in the process list (argv is visible via `ps` / a Windows
// process viewer / `/proc/<pid>/cmdline`; env vars set on a child process
// are not). Verified against the locally installed codex CLI
// (@openai/codex) that an inline `-c mcp_servers.<name>.bearer_token_env_var=
// <VAR>` override is recognized identically to the persisted
// `codex mcp add --bearer-token-env-var` form (`codex mcp list` reports
// "Bearer token" auth for an entry configured this way, entirely in-memory
// — never written to ~/.codex/config.toml).
const codexMCPTokenEnvVar = "CRUSH_CODEX_MCP_TOKEN"

// codexMCPConfigArgs returns the -c inline config override args that
// register crush's MCP server (listening on addr) with codex, with the auth
// token referenced by environment variable name rather than embedded in the
// URL — see codexMCPTokenEnvVar's doc. Extracted as a pure function so the
// no-token-in-argv property can be tested directly, without spawning the
// real MCP server or the codex binary.
func codexMCPConfigArgs(addr string) []string {
	urlNoToken := "http://" + addr + "/mcp"
	return []string{
		"-c", fmt.Sprintf("mcp_servers.crush.url=%q", urlNoToken),
		"-c", "mcp_servers.crush.bearer_token_env_var=" + codexMCPTokenEnvVar,
	}
}

// codexArgs returns a BuildArgs func for a codex CLI model.
func codexArgs(model string) func(bool) []string {
	return func(yolo bool) []string {
		args := []string{"exec", "--json", "-m", model}
		if yolo {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		return args
	}
}

// qwenArgs returns a BuildArgs func for the qwen CLI model.
func qwenArgs() func(bool) []string {
	return func(yolo bool) []string {
		args := []string{
			"--output-format", "stream-json",
			"--include-partial-messages",
		}
		if yolo {
			args = append(args, "--approval-mode", "yolo")
		}
		return args
	}
}

// geminiArgs returns a BuildArgs func for a gemini CLI model.
func geminiArgs(model string) func(bool) []string {
	return func(yolo bool) []string {
		args := []string{
			"-m", model,
			"--output-format", "stream-json",
		}
		if yolo {
			args = append(args, "-y")
		}
		return args
	}
}

// claudeSpec builds a CLI-Claude entry. The model argument is what gets
// passed to `claude --model <X>` — either an alias ("sonnet", "haiku",
// "opus", "fable", which the CLI resolves to its current default) or a
// pinned model id ("claude-opus-4-7", "claude-opus-4-8", …). We no longer
// generate per-effort -thinking variants: the UI's reasoning-effort
// selector (low/medium/high/xhigh/max) is forwarded via context at call
// time and rewrites the `--effort` flag in BuildArgs.
func claudeSpec(modelID, modelName, modelArg string, ctxWindow int64) CLISpec {
	return CLISpec{
		ModelID:        modelID,
		ModelName:      modelName,
		ContextWindow:  ctxWindow,
		Binary:         "claude",
		PromptFlag:     "-p",
		BuildArgs:      claudeArgs(modelArg),
		NewPartParser:  claudePartParser,
		ParseUsageLine: claudeParseUsageLine,
		UseCrushMCP:    true,
		SupportsResume: true,
		ApplyEffort:    applyClaudeEffort,
		EffortLevels:   claudeEffortLevels,
	}
}

// codexSpec builds a codex CLI entry.
//
// ContextWindow is fixed at 272_000 because codex's own embedded registry
// reports that for every model it serves, including the fallback metadata it
// applies to slugs it no longer knows. The previous per-entry 400_000 was a
// 48% overstatement of a number that gates auto-summarization.
//
// effortLevels differs per MODEL, which is why it is a parameter rather than a
// constant: sol/terra accept "ultra", luna stops at "max", the rest at
// "xhigh". A level outside the set is dropped by applyEffort instead of
// reaching the provider as a 400.
func codexSpec(modelID, modelName, modelArg string, effortLevels []string) CLISpec {
	return CLISpec{
		ModelID:             modelID,
		ModelName:           modelName,
		ContextWindow:       272_000,
		Binary:              "codex",
		BuildArgs:           codexArgs(modelArg),
		NewPartParser:       codexPartParser,
		ParseUsageLine:      codexParseUsageLine,
		AlwaysStdin:         true,
		CodexMCPIntegration: true,
		ApplyEffort:         applyCodexEffort,
		EffortLevels:        effortLevels,
	}
}

// geminiSpec builds a gemini CLI entry.
//
// ApplyEffort/EffortLevels are deliberately left nil: the gemini CLI has no
// reasoning-effort option and aborts with "Unknown argument: effort" if one is
// passed, so applyEffort must drop whatever the session stored.
//
// ContextWindow is 1M for every model currently exposed here.
func geminiSpec(modelID, modelName, modelArg string) CLISpec {
	return CLISpec{
		ModelID:              modelID,
		ModelName:            modelName,
		ContextWindow:        1_000_000,
		Binary:               "gemini",
		BuildArgs:            geminiArgs(modelArg),
		NewPartParser:        geminiPartParser,
		ParseUsageLine:       geminiParseUsageLine,
		AlwaysStdin:          true,
		GeminiMCPIntegration: true,
	}
}

// All is the list of hardcoded CLI model specs.
// Add new entries here to register additional CLI-backed models.
var All = []CLISpec{
	// Anthropic's `claude` CLI on PATH. One entry per model family;
	// pinned Opus versions because the operator usually wants a specific
	// generation (4.6/4.7/4.8 differ meaningfully). Sonnet / Haiku /
	// Fable use the aliases so each tab auto-tracks the latest.
	//
	// Alias entries deliberately carry NO version number in their display
	// name: the CLI resolves an alias to whatever it currently considers
	// that family's default, so a hardcoded version goes stale silently.
	// Measured 2026-08-16 against claude 2.1.197 (`--output-format json`
	// reports the resolved id in modelUsage): opus -> claude-opus-4-8,
	// sonnet -> claude-sonnet-5, haiku -> claude-haiku-4-5-20251001,
	// fable -> claude-fable-5. The sonnet entry used to be labelled
	// "Sonnet 4.6" while actually running Sonnet 5.
	claudeSpec("cli-claude-haiku", "Claude Haiku (CLI, latest)", "haiku", 200_000),
	claudeSpec("cli-claude-sonnet", "Claude Sonnet (CLI, latest)", "sonnet", 1_000_000),
	// Opus: alias entry kept so DB rows / atoms (`opus`) referencing the
	// classic ModelID don't dangle, plus three pinned variants the operator
	// can pick explicitly.
	claudeSpec("cli-claude-opus", "Claude Opus (CLI, latest)", "opus", 1_000_000),
	claudeSpec("cli-claude-opus-4-6", "Claude Opus 4.6 (CLI)", "claude-opus-4-6", 1_000_000),
	claudeSpec("cli-claude-opus-4-7", "Claude Opus 4.7 (CLI)", "claude-opus-4-7", 1_000_000),
	claudeSpec("cli-claude-opus-4-8", "Claude Opus 4.8 (CLI)", "claude-opus-4-8", 1_000_000),
	claudeSpec("cli-claude-fable", "Claude Fable (CLI, latest)", "fable", 1_000_000),
	// Claude 5 generation, pinned. The `[1m]` suffix is a real
	// context-window switch the CLI understands, not cosmetic: measured
	// 2026-08-16, claude-opus-5 reports contextWindow=200_000 while
	// claude-opus-5[1m] reports 1_000_000. Note no alias reaches Opus 5 —
	// `opus` still resolves to 4.8 — so these must be explicit.
	//
	// ModelID keeps our `cli-claude-*` slug convention and spells the
	// suffix `-1m` rather than embedding brackets, which would otherwise
	// end up inside `provider/model` strings in config, atoms and the DB.
	claudeSpec("cli-claude-opus-5-1m", "Claude Opus 5 1M (CLI)", "claude-opus-5[1m]", 1_000_000),
	claudeSpec("cli-claude-sonnet-5-1m", "Claude Sonnet 5 1M (CLI)", "claude-sonnet-5[1m]", 1_000_000),
	claudeSpec("cli-claude-fable-5", "Claude Fable 5 (CLI)", "claude-fable-5", 1_000_000),
	// NOT exposed: claude-mythos-5. The id and a `mythos` alias both exist in
	// the CLI, but both return HTTP 404 model_not_found ("It may not exist or
	// you may not have access to it") on this account. An earlier check
	// appeared to pass only because it asserted that modelUsage was non-empty
	// without printing WHICH model answered - a 404 yields an empty
	// modelUsage, so the check was reading its own loop never running as
	// success. Re-add once a ping actually resolves to claude-mythos-5.
	// Google's `gemini` CLI. Model ids are taken from the CLI's own
	// VALID_GEMINI_MODELS set. No effort knob: `gemini --effort high` aborts
	// with "Unknown argument: effort", so these specs leave ApplyEffort nil
	// and applyEffort drops any effort a session happens to carry.
	//
	// cli-gemini-flash is deliberately labelled without a version. It pins
	// gemini-3-flash, but that id now REDIRECTS: the CLI's own response
	// reports the resolved model as gemini-3.5-flash. The entry has been
	// running 3.5 while advertising 3 — the same stale-label class fixed for
	// cli-claude-sonnet. Use cli-gemini-flash-35 to pin 3.5 explicitly.
	geminiSpec("cli-gemini-flash", "Gemini Flash (CLI, latest)", "gemini-3-flash"),
	geminiSpec("cli-gemini-flash-35", "Gemini 3.5 Flash (CLI)", "gemini-3.5-flash"),
	geminiSpec("cli-gemini-flash-lite", "Gemini 3.1 Flash-Lite (CLI)", "gemini-3.1-flash-lite"),
	geminiSpec("cli-gemini-pro", "Gemini 3.1 Pro (CLI)", "gemini-3.1-pro-preview"),
	{
		ModelID:            "cli-qwen",
		ModelName:          "Qwen 3.5 Plus (CLI)",
		ContextWindow:      1_000_000,
		Binary:             "qwen",
		BuildArgs:          qwenArgs(),
		NewPartParser:      claudePartParser,
		ParseUsageLine:     claudeParseUsageLine,
		AlwaysStdin:        true,
		QwenMCPIntegration: true,
	},
	// OpenAI's `codex` CLI. Context windows and effort levels come from the
	// model registry embedded in codex.exe itself (codex-cli 0.147.0), which
	// is authoritative - it is what the binary consults at runtime.
	//
	// All codex models are 272_000, NOT the 400_000 these entries used to
	// claim. ContextWindow drives the auto-summarization threshold, so a 48%
	// overstatement let conversations run well past the real limit.
	codexSpec("cli-codex-sol", "GPT-5.6-Sol (CLI)", "gpt-5.6-sol", codexEffortLevelsUltra),
	codexSpec("cli-codex-terra", "GPT-5.6-Terra (CLI)", "gpt-5.6-terra", codexEffortLevelsUltra),
	codexSpec("cli-codex-luna", "GPT-5.6-Luna (CLI)", "gpt-5.6-luna", codexEffortLevelsMax),
	// gpt-5.5 stops at xhigh. Note the ceiling below only clamps efforts CRUSH
	// sends; it says nothing about codex's own default. If the operator's
	// ~/.codex/config.toml sets a higher model_reasoning_effort (e.g. "max",
	// which is valid for sol/terra), the CLI applies it whenever crush passes
	// none and the API rejects the turn:
	//
	//	Unsupported value: 'max' is not supported with the gpt-5.5 model.
	//	Supported values are: 'none', 'low', 'medium', 'high', 'xhigh'.
	//
	// Reproduced on this machine; passing any supported level explicitly
	// (`crush ping --model local-cli/cli-codex-gpt-5-5@xhigh`) succeeds. crush
	// deliberately does not overwrite a codex config it did not write.
	codexSpec("cli-codex-gpt-5-5", "GPT-5.5 (CLI)", "gpt-5.5", codexEffortLevelsStandard),
	codexSpec("cli-codex-gpt-5-4", "GPT-5.4 (CLI)", "gpt-5.4", codexEffortLevelsStandard),
	codexSpec("cli-codex-gpt-5-2-base", "GPT-5.2 (CLI)", "gpt-5.2", codexEffortLevelsStandard),

	// Slugs that are NO LONGER in codex's registry. Kept rather than deleted
	// so existing session rows and short codes referencing them do not
	// dangle - the same reason the claude list keeps its alias entries.
	//
	// They do not hard-fail: codex warns "Model metadata for `X` not found.
	// Defaulting to fallback metadata; this can degrade performance" and runs
	// anyway, which is easy to miss. The display name says so, because an
	// entry that silently degrades is worse than one that refuses.
	codexSpec("cli-codex", "Codex gpt-5.3-codex (CLI, unsupported)", "gpt-5.3-codex", codexEffortLevelsStandard),
	codexSpec("cli-codex-gpt-5-2", "Codex gpt-5.2-codex (CLI, unsupported)", "gpt-5.2-codex", codexEffortLevelsStandard),
	codexSpec("cli-codex-max", "Codex gpt-5.1-codex-max (CLI, unsupported)", "gpt-5.1-codex-max", codexEffortLevelsStandard),
	codexSpec("cli-codex-mini", "Codex gpt-5.1-codex-mini (CLI, unsupported)", "gpt-5.1-codex-mini", codexEffortLevelsStandard),
}
