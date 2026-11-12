import { ws } from "../net/client";
import type { ModelRole } from "../state/ConfigCarburetor";
import { configCarburetor } from "../state/config";

/**
 * Drops a model from its role recent list and persists the removal to the server.
 *
 * @param role - model slot
 * @param modelKey - provider:::model key
 */
export function removeRecentModel(role: ModelRole, modelKey: string): void {
  configCarburetor.removeRecent(role, modelKey);

  // Persist removal to server
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("remove_recent_model", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}
