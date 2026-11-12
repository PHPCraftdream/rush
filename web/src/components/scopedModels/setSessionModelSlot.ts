import { ws } from "../../ws";
import type { Slot } from "./scopedModelsTypes";

// setSessionModelSlot writes ONE session model slot (smart/fast/worker/
// reviewer), leaving every other slot untouched — same nil-means-untouched
// wire convention as clearSessionModelSlot (task #467) and ModelSelector's
// onSelect (task #461).

/**
 * Writes one session model slot.
 *
 * @param sessionID - the session
 * @param slot - the slot to write
 * @param provider - provider id
 * @param model - model id
 * @param effort - reasoning effort, "" for none
 */
export function setSessionModelSlot(sessionID: string, slot: Slot, provider: string, model: string, effort: string) {
  ws.send("set_session_models", {
    sessionID,
    [`${slot}Model`]: { provider, model, reasoning_effort: effort },
  });
}
