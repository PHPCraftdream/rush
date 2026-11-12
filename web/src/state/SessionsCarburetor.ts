import { Carburetor } from "@react-carburetor/core";
import type { Session } from "../types";

/** Sessions known to this tab and the one the chat is showing. */
export interface ISessionsState {
  sessions: Session[];
  activeSessionID: string | null;
}

/** Live session pushes that were applied while a sessions_list request was in flight. */
export interface ISessionsLiveDelta {
  upserts: Session[];
  deletedIDs: string[];
}

/**
 * Replaces the sessions list in the draft.
 *
 * @param draft - state to change
 * @param sessions - the new list
 */
const writeSessions = (draft: ISessionsState, sessions: Session[]): void => {
  draft.sessions = sessions;
};

/**
 * Writes the active session id into the draft.
 *
 * @param draft - state to change
 * @param id - the active session, or null for none
 */
const writeActive = (draft: ISessionsState, id: string | null): void => {
  draft.activeSessionID = id;
};

/**
 * Overwrites fields of one session in the draft.
 *
 * @param draft - state to change
 * @param index - position of the session in the list
 * @param patch - fields to overwrite
 */
const writePatch = (draft: ISessionsState, index: number, patch: Readonly<Record<string, unknown>>): void => {
  Object.assign(draft.sessions[index], patch);
};

/**
 * Replays live pushes over a snapshot: deletions first, then upserts
 * (replace in place, or prepend when new).
 *
 * @param snapshot - the sessions_list rows
 * @param live - live pushes recorded since the request was sent
 */
const replayLiveDelta = (snapshot: readonly Session[], live: ISessionsLiveDelta): Session[] => {
  let next = [...snapshot];
  for (const id of live.deletedIDs) {
    next = next.filter((s) => s.ID !== id);
  }
  for (const s of live.upserts) {
    const idx = next.findIndex((x) => x.ID === s.ID);
    if (idx === -1) {
      next = [s, ...next];
    } else {
      next = [...next];
      next[idx] = s;
    }
  }
  return next;
};

/** Holds the sessions list and the active session id. */
export class SessionsCarburetor extends Carburetor<ISessionsState> {
  /**
   * Replaces the list wholesale.
   *
   * @param sessions - the new list
   */
  public setSessions(sessions: Session[]): void {
    this.update(writeSessions, sessions);
  }

  /**
   * Applies a sessions_list snapshot, replaying live pushes that raced the request (task #690).
   *
   * With an empty delta this is a plain wholesale replace, the convergent
   * behavior the 5s poll depends on. Returns the reconciled list so routing
   * decisions downstream use it, not the raw snapshot.
   *
   * @param sessions - the snapshot rows
   * @param live - live pushes recorded since the request was sent
   */
  public applySnapshot(sessions: readonly Session[], live: ISessionsLiveDelta): Session[] {
    const next = replayLiveDelta(sessions, live);
    this.update(writeSessions, next);
    return next;
  }

  /**
   * Inserts a session at the front, or replaces the row with the same id.
   *
   * @param s - the session pushed by the server
   */
  public upsert(s: Session): void {
    const list = this.getData().sessions;
    const idx = list.findIndex((x) => x.ID === s.ID);
    if (idx === -1) {
      this.update(writeSessions, [s, ...list]);
    } else {
      const next = [...list];
      next[idx] = s;
      this.update(writeSessions, next);
    }
  }

  /**
   * Drops the session row (not its dependent state; see removeSession action).
   *
   * @param id - session id
   */
  public remove(id: string): void {
    this.update(writeSessions, this.getData().sessions.filter((s) => s.ID !== id));
  }

  /**
   * Changes the active session.
   *
   * @param id - the session to show, or null for none
   */
  public setActiveSessionID(id: string | null): void {
    this.update(writeActive, id);
  }

  /**
   * Optimistically overwrites fields of one session; a no-op for an unknown id.
   *
   * @param sessionID - session to patch
   * @param patch - fields to overwrite
   * @returns whether the session was found
   */
  public patchSession(sessionID: string, patch: Readonly<Record<string, unknown>>): boolean {
    const idx = this.getData().sessions.findIndex((s) => s.ID === sessionID);
    if (idx === -1) return false;
    this.update(writePatch, idx, patch);
    return true;
  }

  /**
   * Looks a session up without subscribing.
   *
   * @param sessionID - session id
   */
  public findSession(sessionID: string | null | undefined): Session | undefined {
    if (!sessionID) return undefined;
    return this.getData().sessions.find((s) => s.ID === sessionID);
  }
}
