import { Carburetor } from "@react-carburetor/core";

/** Status of the babysitter loop (see SitterRunner). */
export interface SitterStatus {
  running: boolean;
  sessionID?: string;
  intervalMin?: number;
  startedAt?: number;
  lastTickAt?: number;
  lastNudgeAt?: number;
}

/** The sitter status, wrapped so the whole object is replaced per change. */
export interface ISitterState {
  sitter: SitterStatus;
}

/** localStorage key under which a running sitter survives a page reload. */
const LS_KEY = "rushSitter";

/**
 * Replaces the status in the draft.
 *
 * @param draft - state to change
 * @param next - the new status
 */
const writeStatus = (draft: ISitterState, next: SitterStatus): void => {
  draft.sitter = next;
};

/** Holds the sitter status; a running sitter is mirrored to localStorage. */
export class SitterCarburetor extends Carburetor<ISitterState> {
  /** The saved sitter (session and interval) from localStorage, or null. Throws on corrupt JSON. */
  public static readSaved(): { sessionID?: string; intervalMin?: number } | null {
    const raw = localStorage.getItem(LS_KEY);
    return raw ? (JSON.parse(raw) as { sessionID?: string; intervalMin?: number }) : null;
  }

  /** Forgets the saved sitter. */
  public static clearSaved(): void {
    localStorage.removeItem(LS_KEY);
  }

  /**
   * Stores a status and persists it (or forgets it when the sitter is not running).
   *
   * @param next - the new status
   */
  public setStatus(next: SitterStatus): void {
    this.update(writeStatus, next);
    if (next.running && next.sessionID && next.intervalMin) {
      localStorage.setItem(LS_KEY, JSON.stringify({ sessionID: next.sessionID, intervalMin: next.intervalMin }));
    } else {
      localStorage.removeItem(LS_KEY);
    }
  }
}
