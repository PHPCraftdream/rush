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

// z.ai GLM-5.x (pre-5.3) only exposes High / Max natively (see docs.z.ai/
// devpack/latest-model and the MarkTechPost launch coverage). The chevron
// selector cycles through just these two; the backend mirrors them onto the
// provider's reasoning_effort field.
export const EFFORT_LEVELS_ZAI = ["high", "max"] as const;

// GLM-5.3 and GLM-5.3-Flash broke that pattern — reasoning can't be disabled
// at all and the model instead exposes low/high/max. See the verification
// and doc quotes on zai53ReasoningLevels in internal/cmd/models_atoms.go
// (the backend counterpart this must stay in sync with).
export const EFFORT_LEVELS_ZAI53 = ["low", "high", "max"] as const;

// Returns true for GLM-5.3 / GLM-5.3-Flash specifically (not other GLM-5.x),
// regardless of which provider key the model lives under. Matches the exact
// id and the "[1m]"-suffixed context-window variant.
export function isZAI53Model(_provider: string, model: string): boolean {
  return /^glm-5\.3(-flash)?(\[|$)/i.test(model);
}

// Returns true for any OTHER GLM-5.x model regardless of which provider key
// it lives under — users sometimes wire z.ai via a custom OpenAI-compat
// provider (id "z-ai" / "zhipu" / etc.), so matching the model id is the
// robust signal. The "[1m]" suffix variant (glm-5.2[1m]) is also covered.
// Older GLM-4.x families fall through to the binary thinking on/off in the
// coordinator and don't get the selector. Excludes GLM-5.3/5.3-Flash, which
// have their own, larger vocabulary — see isZAI53Model above.
export function isZAIReasoningModel(_provider: string, model: string): boolean {
  return !isZAI53Model(_provider, model) && /^glm-5(\.|-|\[|$)/i.test(model);
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
// Codex is NOT listed yet even though its CLI does accept an effort
// (`-c model_reasoning_effort=`), because its levels are per-model — codex's
// own registry stops gpt-5.5 at "xhigh" while gpt-5.6-sol accepts "ultra" —
// and the frontend has no source for that table. Hardcoding a third copy of
// per-model levels here is what this module exists to prevent; wire it through
// from the backend spec when codex effort is exposed.
export function effortLevelsFor(provider: string, model: string): readonly string[] | null {
  if (isZAI53Model(provider, model)) return EFFORT_LEVELS_ZAI53;
  if (isZAIReasoningModel(provider, model)) return EFFORT_LEVELS_ZAI;
  if (isCLIClaudeModel(provider, model)) return EFFORT_LEVELS;
  return null;
}

// supportsEffort is the boolean form, for callers that only need to decide
// whether to render a control.
export function supportsEffort(provider: string, model: string): boolean {
  return effortLevelsFor(provider, model) !== null;
}

// defaultEffortFor is the level to show when the session has none stored.
// Claude CLI keeps the legacy "medium"; every z.ai GLM-5.x model (including
// the 5.3-tier's low/high/max vocabulary) defaults to "high" — the fork's
// existing convention, not z.ai's own doc default of "max" for the 5.3 tier
// (Max stays opt-in for heavy work).
export function defaultEffortFor(provider: string, model: string): string {
  return isZAI53Model(provider, model) || isZAIReasoningModel(provider, model) ? "high" : "medium";
}

// clampEffort maps a stored effort onto something this model accepts.
//
// Returns null when the model takes no effort at all, which callers must treat
// as "clear the stored value", not "keep it": a session that moves from Claude
// to gemini leaves behind an effort the new model cannot use, and that stale
// value is what used to kill the run outright.
export function clampEffort(provider: string, model: string, stored: string): string | null {
  const levels = effortLevelsFor(provider, model);
  if (levels === null) return null;
  if (stored && levels.includes(stored)) return stored;
  const fallback = defaultEffortFor(provider, model);
  return levels.includes(fallback) ? fallback : levels[0];
}
