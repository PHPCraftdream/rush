import type { TReadonly } from "@react-carburetor/core";
import type { Session } from "../../types";

/**
 * Looks a session up by id; the loop keeps the read subscription to ids and the one hit.
 *
 * @param sessions - the sessions list
 * @param id - the session id, or null when none is active
 */
export const findSession = (sessions: TReadonly<Session[]>, id: string | null): TReadonly<Session> | null => {
  for (let i = 0; i < sessions.length; i++) {
    if (sessions[i].ID === id) return sessions[i];
  }
  return null;
};
