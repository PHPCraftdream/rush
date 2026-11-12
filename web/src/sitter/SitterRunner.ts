import { ws } from "../net/client";
import type { Todo } from "../types";
import { busyCarburetor } from "../state/busy";
import { sessionsCarburetor } from "../state/sessions";
import { SitterCarburetor } from "../state/SitterCarburetor";
import { sitterCarburetor } from "../state/sitter";

/**
 * Whether a todo still needs work.
 *
 * @param t - the todo
 */
const isOpenTodo = (t: Todo): boolean => t.status === "pending" || t.status === "in_progress";

/** Interval used when the operator gives none or an invalid one. */
const DEFAULT_MINUTES = 10;

/**
 * Short resume prompt sent on each "stalled + has work" tick.
 *
 * Kept in Russian because that is the working language of this fork operator;
 * the agent picks up the conversation language from the history.
 */
const RESUME_PROMPT = "продолжай";

/**
 * "Babysitter" loop that watches a session and kicks it back to life when the agent stalls.
 *
 * Use case: long-running tasks where the upstream provider (z.ai, Claude, ...)
 * intermittently stops streaming, the watchdog has already given up, and the
 * operator wants the run to auto-resume without pasting "continue" every 10
 * minutes.
 *
 * Rules per tick:
 * 1. If the watched session has zero outstanding todos (no pending /
 *    in_progress), the sitter shuts itself down: the work is done.
 * 2. If the session is currently busy, it is still running: no nudge.
 * 3. If the session is idle AND has pending work, a short resume prompt is
 *    sent as if the operator had typed it, through the normal WS send_message
 *    path so it appears in history.
 *
 * Toggled via the chat input: `/sitter` (default 10 min), `/sitter 5` (5 min),
 * `/sitter off`. State persists via localStorage so a page reload does not drop
 * the sitter (restored on app mount).
 */
export class SitterRunner {
  private timer: number | undefined;

  /** One interval tick: retire, skip or nudge depending on the watched session. */
  private tick = (): void => {
    const s = sitterCarburetor.getData().sitter;
    if (!s.running || !s.sessionID) return;
    const sess = sessionsCarburetor.findSession(s.sessionID);
    if (!sess) {
      // session vanished (deleted in another tab): stop quietly.
      this.stop();
      return;
    }
    // Read-only follow mode: another live rush process drives this session;
    // any send_message we fire would be ignored at best and confuse history
    // at worst. Skip: the sitter stays armed for if/when ownership reverts.
    if (sess.OwnedExternal) {
      sitterCarburetor.setStatus({ ...s, lastTickAt: Date.now() });
      return;
    }
    const todos = sess.Todos ?? [];
    const hasPending = todos.some(isOpenTodo);
    if (!hasPending) {
      // Job done: the sitter retires itself.
      this.stop();
      return;
    }
    if (busyCarburetor.isBusy(s.sessionID)) {
      // Agent is still chewing on something. Do not nudge: give it time.
      sitterCarburetor.setStatus({ ...s, lastTickAt: Date.now() });
      return;
    }
    // Idle + has work: send a resume prompt as a regular user message.
    ws.send("send_message", { sessionID: s.sessionID, content: RESUME_PROMPT });
    sitterCarburetor.setStatus({ ...s, lastTickAt: Date.now(), lastNudgeAt: Date.now() });
  };

  /**
   * Starts (or restarts) the sitter on a session.
   *
   * @param sessionID - session to watch
   * @param intervalMin - tick interval in minutes; non-positive means the default
   */
  public start(sessionID: string, intervalMin: number): void {
    if (!sessionID) return;
    const mins = intervalMin > 0 ? intervalMin : DEFAULT_MINUTES;
    if (this.timer !== undefined) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    const now = Date.now();
    sitterCarburetor.setStatus({
      running: true,
      sessionID,
      intervalMin: mins,
      startedAt: now,
      lastTickAt: now,
    });
    this.timer = window.setInterval(this.tick, mins * 60 * 1000);
  }

  /** Stops the sitter. */
  public stop(): void {
    if (this.timer !== undefined) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    sitterCarburetor.setStatus({ running: false });
  }

  /**
   * Re-arms the sitter from localStorage on app mount. Call once near app bootstrap.
   *
   * If the saved session no longer exists on the server, the first tick quietly drops the sitter.
   */
  public restore(): void {
    try {
      const saved = SitterCarburetor.readSaved();
      if (saved?.sessionID && saved.intervalMin && saved.intervalMin > 0) {
        this.start(saved.sessionID, saved.intervalMin);
      }
    } catch {
      // Corrupted state: wipe and move on.
      SitterCarburetor.clearSaved();
    }
  }

  /**
   * Parses a slash-command typed in the chat input.
   *
   * Returns true if the input was a sitter command (the caller suppresses
   * sending it to the server); false if it was something else. Forms:
   * `/sitter` toggles (ON to off, OFF to start with the default 10m),
   * `/sitter N` starts or restarts with N minutes (a positive integer),
   * `/sitter off|stop` stops explicitly.
   *
   * @param text - the composer text
   */
  public handleCommand(text: string): boolean {
    const trimmed = text.trim();
    if (!/^\/sitter(\b|$)/i.test(trimmed)) return false;
    const parts = trimmed.split(/\s+/);
    const arg = parts[1]?.toLowerCase();
    const targetSession = sitterCarburetor.getData().sitter.sessionID || sessionsCarburetor.getData().activeSessionID;
    if (!targetSession) return true; // nothing to attach to, swallow silently
    if (arg === "off" || arg === "stop" || arg === "0") {
      this.stop();
      return true;
    }
    if (!arg) {
      if (sitterCarburetor.getData().sitter.running) this.stop();
      else this.start(targetSession, DEFAULT_MINUTES);
      return true;
    }
    const n = parseInt(arg, 10);
    if (!Number.isFinite(n) || n <= 0) {
      // unrecognized argument: treat as start with default
      this.start(targetSession, DEFAULT_MINUTES);
      return true;
    }
    this.start(targetSession, n);
    return true;
  }
}
