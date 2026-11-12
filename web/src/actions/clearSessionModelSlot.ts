import { ws } from "../net/client";
import { sessionsCarburetor } from "../state/sessions";

const CLEARED_FIELDS: Record<string, string> = {
  smart: "SmartModel",
  fast: "FastModel",
  worker: "WorkerModel",
  reviewer: "ReviewerModel",
};

/**
 * Removes the session explicit override for ONE slot, falling back to the folder/system default (task #467).
 *
 * This is NOT the same as setting a model with an empty key: an empty/null key
 * means "don't touch this slot" (it maps to JS `null`, which is omitted from the
 * WS payload and read as "leave alone" server-side, see handleSetSessionModels).
 * Clearing instead requires a NON-null object with an explicit empty
 * provider/model, which internal/session UpdateModels reads as "wipe this
 * override". Only ONE slot is sent; the other slots keep their override.
 *
 * @param sessionID - session id
 * @param modelType - the slot to clear
 */
export function clearSessionModelSlot(sessionID: string, modelType: "smart" | "fast" | "worker" | "reviewer"): void {
  const prefix = CLEARED_FIELDS[modelType];
  sessionsCarburetor.patchSession(sessionID, { [`${prefix}Provider`]: "", [`${prefix}ID`]: "" });

  ws.send("set_session_models", {
    sessionID,
    [`${modelType}Model`]: { provider: "", model: "" },
  });
}
