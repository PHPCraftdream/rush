import { isCLIClaudeModel } from "./isCLIClaudeModel.ts";
import type { EffortCapabilities } from "./effortLevelsFor.ts";

/**
 * Shows a provider-advertised default only when it belongs to that model's advertised ladder.
 *
 * Claude CLI retains its existing default.
 *
 * @param provider - provider id
 * @param model - model id
 * @param capabilities - provider-reported metadata
 */
export function defaultEffortFor(provider: string, model: string, capabilities?: EffortCapabilities): string {
  if (capabilities?.reasoningLevels?.length) {
    const proposed = capabilities.defaultReasoningEffort ?? "";
    return capabilities.reasoningLevels.includes(proposed) ? proposed : "";
  }
  return isCLIClaudeModel(provider, model) ? "medium" : "";
}
