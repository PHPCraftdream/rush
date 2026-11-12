import { ws } from "../net/client";
import type { ModelRole } from "../state/ConfigCarburetor";
import { configCarburetor } from "../state/config";

/**
 * Moves a model to the front of its role recent list and syncs it to the backend.
 *
 * @param role - model slot
 * @param modelKey - provider:::model key
 */
export function trackModelUsage(role: ModelRole, modelKey: string): void {
  configCarburetor.trackRecent(role, modelKey);

  // Sync to backend: parse modelKey into provider and model
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("track_model_usage", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}
