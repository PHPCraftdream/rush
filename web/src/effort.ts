// Reasoning-effort capability, shared by every UI that offers the setting.
//
// This lived as private helpers inside ModelSelector.tsx. It moved here when a
// second surface (the Default-models modal) needed the same rules, because a
// second hand-written copy is exactly how a model that cannot take an effort
// ends up being offered one.
//
// That is not a cosmetic concern. Verified against the installed binaries on
// 2026-08-16:
//
//   gemini --effort high ...   -> Unknown argument: effort
//   qwen   --effort high ...   -> Unknown argument: effort
//   codex exec --effort high   -> error: unexpected argument '--effort' found
//
// The backend now drops an effort a CLI cannot accept
// (internal/agent/cliprovider/effort.go), but the UI must not offer it in the
// first place — and a value written at SYSTEM scope in the Default-models
// modal is inherited by every future session in every workspace, so a wrong
// one there is far wider-reaching than a per-session mistake.

// Claude CLI: `claude --help` documents low|medium|high|xhigh|max.
export const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;

const EFFORT_LABELS: Record<string, string> = {
  none: "OFF", minimal: "MIN", low: "L", medium: "M",
  high: "H", xhigh: "X", max: "XX", ultra: "U",
};

export function effortLabel(effort: string): string {
  return EFFORT_LABELS[effort] ?? effort.toUpperCase();
}

// Returns true if the model is a CLI Claude model (supports reasoning_effort).
export function isCLIClaudeModel(provider: string, model: string): boolean {
  return provider === "local-cli" && (model.startsWith("cli-claude-") || model.startsWith("cli-npx-claude-"));
}

// effortLevelsFor returns the levels this model accepts, or null when it has
// no reasoning-effort knob at all.
//
// null is deliberately distinct from an empty array: callers must hide the
// control entirely rather than render an empty dropdown, and must not persist
// an effort for such a model.
//
// Provider-reported levels are the source of truth. Missing metadata means no
// picker; never infer a ladder from a model name.
export interface EffortCapabilities {
  reasoningLevels?: readonly string[];
  defaultReasoningEffort?: string;
}

export function effortLevelsFor(provider: string, model: string, capabilities?: EffortCapabilities): readonly string[] | null {
  if (capabilities?.reasoningLevels?.length) return capabilities.reasoningLevels;
  if (isCLIClaudeModel(provider, model)) return EFFORT_LEVELS;
  return null;
}

// supportsEffort is the boolean form, for callers that only need to decide
// whether to render a control.
export function supportsEffort(provider: string, model: string, capabilities?: EffortCapabilities): boolean {
  return effortLevelsFor(provider, model, capabilities) !== null;
}

// defaultEffortFor shows a provider-advertised default only when it belongs to
// that model's advertised ladder. Claude CLI retains its existing default.
export function defaultEffortFor(provider: string, model: string, capabilities?: EffortCapabilities): string {
  if (capabilities?.reasoningLevels?.length) {
    const proposed = capabilities.defaultReasoningEffort ?? "";
    return capabilities.reasoningLevels.includes(proposed) ? proposed : "";
  }
  return isCLIClaudeModel(provider, model) ? "medium" : "";
}

// clampEffort maps a stored effort onto something this model accepts.
//
// Returns null when the model takes no effort at all, which callers must treat
// as "clear the stored value", not "keep it": a session that moves from Claude
// to gemini leaves behind an effort the new model cannot use, and that stale
// value is what used to kill the run outright.
export function clampEffort(provider: string, model: string, stored: string, capabilities?: EffortCapabilities): string | null {
  const levels = effortLevelsFor(provider, model, capabilities);
  if (levels === null) return null;
  if (stored && levels.includes(stored)) return stored;
  const fallback = defaultEffortFor(provider, model, capabilities);
  return levels.includes(fallback) ? fallback : levels[0];
}
