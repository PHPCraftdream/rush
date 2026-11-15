import { defaultEffortFor } from "./defaultEffortFor.ts";
import { effortLevelsFor } from "./effortLevelsFor.ts";
import type { EffortCapabilities } from "./effortLevelsFor.ts";

/**
 * Maps a stored effort onto something this model accepts.
 *
 * Returns null when the model takes no effort at all, which callers must treat
 * as "clear the stored value", not "keep it": a session that moves from Claude
 * to gemini leaves behind an effort the new model cannot use, and that stale
 * value is what used to kill the run outright.
 *
 * @param provider - provider id
 * @param model - model id
 * @param stored - the effort stored so far
 * @param capabilities - provider-reported metadata
 */
export function clampEffort(
  provider: string,
  model: string,
  stored: string,
  capabilities?: EffortCapabilities,
): string | null {
  const levels = effortLevelsFor(provider, model, capabilities);
  if (levels === null) return null;
  if (stored && levels.includes(stored)) return stored;
  const fallback = defaultEffortFor(provider, model, capabilities);
  return levels.includes(fallback) ? fallback : levels[0];
}
