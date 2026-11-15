import { Carburetor } from "@react-carburetor/core";

/** Per-session agent activity flags. */
export interface IBusyState {
  /** Sessions whose agent is running a turn (unique ids; converted to a Set at the read edge). */
  busySessions: string[];
  /** Sessions where the user has queued a compact/summarise request. */
  summarizeQueued: string[];
}

/**
 * Replaces the busy set in the draft.
 *
 * @param draft - state to change
 * @param next - the new set
 */
const writeBusy = (draft: IBusyState, next: string[]): void => {
  draft.busySessions = next;
};

/**
 * Replaces the queued-summarize set in the draft.
 *
 * @param draft - state to change
 * @param next - the new set
 */
const writeSummarize = (draft: IBusyState, next: string[]): void => {
  draft.summarizeQueued = next;
};

/** Holds which sessions are busy and which have a summarize request queued. */
export class BusyCarburetor extends Carburetor<IBusyState> {
  /**
   * Marks a session busy or idle.
   *
   * @param sessionID - session id
   * @param busy - whether its agent is running
   */
  public setBusy(sessionID: string, busy: boolean): void {
    const s = new Set(this.getData().busySessions);
    if (busy) s.add(sessionID);
    else s.delete(sessionID);
    this.update(writeBusy, [...s]);
  }

  /**
   * Replaces the whole busy set.
   *
   * @param next - the new set
   */
  public setBusySessions(next: Set<string>): void {
    this.update(writeBusy, [...next]);
  }

  /**
   * Whether the session's agent is running.
   *
   * @param sessionID - session id
   */
  public isBusy(sessionID: string): boolean {
    return this.getData().busySessions.includes(sessionID);
  }

  /**
   * Marks a session's summarize request as queued or not.
   *
   * @param sessionID - session id
   * @param queued - whether a request is queued
   */
  public setSummarizeQueued(sessionID: string, queued: boolean): void {
    const s = new Set(this.getData().summarizeQueued);
    if (queued) s.add(sessionID);
    else s.delete(sessionID);
    this.update(writeSummarize, [...s]);
  }
}
