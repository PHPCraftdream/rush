import { ws } from "../net/client";
import type { ModelRole } from "../state/ConfigCarburetor";
import { sessionsCarburetor } from "../state/sessions";
import { roleFieldPrefix } from "./roleFieldPrefix";

/**
 * Sets exactly ONE role session override, leaving the other three roles untouched.
 *
 * The set_session_models wire convention (task #461/#466): an omitted
 * `<role>Model` key means "don't touch this slot", so the other three are
 * simply never included in the payload. Generalizes the old two-arg
 * (smart, fast) setter to all four roles (task #1061).
 *
 * @param sessionID - session id
 * @param role - model slot
 * @param key - provider:::model key
 */
export function setSessionModel(sessionID: string, role: ModelRole, key: string): void {
  const idx = key.indexOf(":::");
  if (idx === -1) return;
  const provider = key.slice(0, idx);
  const model = key.slice(idx + 3);
  const prefix = roleFieldPrefix(role);

  // Optimistic local update so the UI reflects the change immediately.
  sessionsCarburetor.patchSession(sessionID, { [`${prefix}ModelProvider`]: provider, [`${prefix}ModelID`]: model });

  ws.send("set_session_models", { sessionID, [`${role}Model`]: { provider, model } });
}
