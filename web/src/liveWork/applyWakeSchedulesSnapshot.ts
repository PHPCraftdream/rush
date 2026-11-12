import { wakeSchedulesCarburetor } from "../state/wakeSchedules";
import type { SessionWakeSchedulesPayload } from "../types";
import { isStaleWakeSchedulesSnapshot } from "./isStaleWakeSchedulesSnapshot";

/**
 * Applies a full session_wake_schedules snapshot; always replaces, never merges, per the wire contract.
 *
 * @param payload - the snapshot
 * @param requestID - correlation id of the reply, when it answers a request
 */
export function applyWakeSchedulesSnapshot(payload: SessionWakeSchedulesPayload, requestID?: string): void {
  if (isStaleWakeSchedulesSnapshot(payload.sessionID, requestID)) return;
  wakeSchedulesCarburetor.setSchedules(payload.sessionID, payload.schedules ?? []);
}
