import { effortLevelsFor } from "./effortLevelsFor.ts";
import type { EffortCapabilities } from "./effortLevelsFor.ts";

/**
 * Boolean form of effortLevelsFor, for callers that only decide whether to render a control.
 *
 * @param provider - provider id
 * @param model - model id
 * @param capabilities - provider-reported metadata
 */
export function supportsEffort(provider: string, model: string, capabilities?: EffortCapabilities): boolean {
  return effortLevelsFor(provider, model, capabilities) !== null;
}
