import { Carburetor } from "@react-carburetor/core";
import type { WakeScheduleItem } from "../types";

/** Wake schedules of each session, delivered as full session_wake_schedules snapshots. */
export interface IWakeSchedulesState {
  bySession: Record<string, WakeScheduleItem[]>;
}

/**
 * Replaces the schedule table in the draft.
 *
 * @param draft - state to change
 * @param next - the new table
 */
const writeTable = (draft: IWakeSchedulesState, next: Record<string, WakeScheduleItem[]>): void => {
  draft.bySession = next;
};

/** Holds the Schedules tab data and the newest request id per session. */
export class WakeSchedulesCarburetor extends Carburetor<IWakeSchedulesState> {
  private readonly latestRequestBySession = new Map<string, string>();

  /**
   * Replaces a session's schedules; always replaces, never merges, per the wire contract.
   *
   * @param sessionID - session id
   * @param items - the snapshot rows
   */
  public setSchedules(sessionID: string, items: WakeScheduleItem[]): void {
    this.update(writeTable, { ...this.getData().bySession, [sessionID]: items });
  }

  /**
   * Records the newest snapshot request for a session.
   *
   * Kept through reply delivery so a late response cannot overwrite a newer snapshot.
   *
   * @param sessionID - session id
   * @param requestID - correlation id of the request just sent
   */
  public trackRequest(sessionID: string, requestID: string): void {
    this.latestRequestBySession.set(sessionID, requestID);
  }

  /** The newest request id per session. */
  public latestRequests(): ReadonlyMap<string, string> {
    return this.latestRequestBySession;
  }
}
