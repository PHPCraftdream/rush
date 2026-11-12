import { ws } from "../net/client";
import type { ModelRole } from "../state/ConfigCarburetor";
import { sessionsCarburetor } from "../state/sessions";
import { roleFieldPrefix } from "./roleFieldPrefix";

/**
 * Sets ONE role reasoning effort, re-sending that role stored provider/model alongside it.
 *
 * The backend backfills an omitted provider/model within the SAME slot, but the
 * slot own effort write needs its own provider/model present (see
 * handleSetSessionModels resolveEffortPair). Generalizes the old two-arg
 * setter to all four roles (task #1061).
 *
 * @param sessionID - session id
 * @param role - model slot
 * @param effort - the new effort; empty clears it
 */
export function setSessionRoleEffort(sessionID: string, role: ModelRole, effort: string): void {
  const prefix = roleFieldPrefix(role);
  const found = sessionsCarburetor.findSession(sessionID);
  if (!found) return;
  const session = found as unknown as Record<string, string>;
  const provider = session[`${prefix}ModelProvider`];
  const model = session[`${prefix}ModelID`];

  sessionsCarburetor.patchSession(sessionID, { [`${prefix}ModelReasoningEffort`]: effort });

  ws.send("set_session_models", {
    sessionID,
    [`${role}Model`]: {
      provider,
      model,
      reasoning_effort: effort || undefined,
    },
  });
}
