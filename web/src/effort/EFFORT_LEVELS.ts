/**
 * Reasoning-effort ladder of the Claude CLI (`claude --help` documents low|medium|high|xhigh|max).
 *
 * Effort capability is shared by every UI that offers the setting. A second
 * hand-written copy of these rules is how a model that cannot take an effort
 * ends up being offered one. Verified against the installed binaries on 2026-08-16:
 * `gemini --effort high` and `qwen --effort high` fail with "Unknown argument:
 * effort", `codex exec --effort high` with "unexpected argument '--effort'".
 * The backend drops an effort a CLI cannot accept
 * (internal/agent/cliprovider/effort.go), but the UI must not offer it in the
 * first place; a value written at SYSTEM scope in the Default-models modal is
 * inherited by every future session in every workspace.
 */
export const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;
