import { clampEffort, defaultEffortFor } from "../../effort";
import type { ModelItem } from "../modelList/modelListTypes";

/**
 * The effort to store when a model is picked: the stored one clamped to the new model's ladder,
 * or the model's default when none was stored.
 *
 * @param provider - provider id of the picked model
 * @param model - picked model id
 * @param stored - the previously stored effort, "" for none
 * @param capabilities - the picked model's catalog entry, if any
 */
export function pickedEffort(provider: string, model: string, stored: string, capabilities?: ModelItem): string {
  if (!stored) return defaultEffortFor(provider, model, capabilities);
  return clampEffort(provider, model, stored, capabilities) ?? "";
}
