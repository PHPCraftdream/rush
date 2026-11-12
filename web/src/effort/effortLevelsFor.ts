import { EFFORT_LEVELS } from "./EFFORT_LEVELS.ts";
import { isCLIClaudeModel } from "./isCLIClaudeModel.ts";

/** Provider-reported effort metadata of a model. */
export interface EffortCapabilities {
  reasoningLevels?: readonly string[];
  defaultReasoningEffort?: string;
}

/**
 * Returns the levels this model accepts, or null when it has no reasoning-effort knob at all.
 *
 * null is deliberately distinct from an empty array: callers must hide the
 * control entirely rather than render an empty dropdown, and must not persist
 * an effort for such a model. Provider-reported levels are the source of truth.
 * Missing metadata means no picker; never infer a ladder from a model name.
 *
 * @param provider - provider id
 * @param model - model id
 * @param capabilities - provider-reported metadata
 */
export function effortLevelsFor(
  provider: string,
  model: string,
  capabilities?: EffortCapabilities,
): readonly string[] | null {
  if (capabilities?.reasoningLevels?.length) return capabilities.reasoningLevels;
  if (isCLIClaudeModel(provider, model)) return EFFORT_LEVELS;
  return null;
}
