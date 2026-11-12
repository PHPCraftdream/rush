import { Carburetor } from "@react-carburetor/core";
import type { LiveWorkItem, SessionLiveWorkPayload } from "../types";

/** One session's live commands and agents. */
export interface SessionLiveWork {
  commands: LiveWorkItem[];
  agents: LiveWorkItem[];
}

/** Live-work panel data (task #1059), keyed by session id. */
export interface ILiveWorkState {
  bySession: Record<string, SessionLiveWork>;
}

/**
 * Replaces the snapshot table in the draft.
 *
 * @param draft - state to change
 * @param next - the new table
 */
const writeTable = (draft: ILiveWorkState, next: Record<string, SessionLiveWork>): void => {
  draft.bySession = next;
};

/** Holds the live-work snapshots pushed by the server. */
export class LiveWorkCarburetor extends Carburetor<ILiveWorkState> {
  /**
   * Applies a full session_live_work snapshot (push or get_session_live_work reply).
   *
   * Always replaces, never merges, per the wire contract. complete===false
   * arrives only from the batch-ON path (a partial read); the panel is kept
   * as-is because an incomplete read must never erase live work.
   *
   * @param payload - the snapshot
   */
  public applySnapshot(payload: SessionLiveWorkPayload): void {
    if (payload.complete === false) return;
    this.update(writeTable, {
      ...this.getData().bySession,
      [payload.sessionID]: { commands: payload.commands ?? [], agents: payload.agents ?? [] },
    });
  }
}
