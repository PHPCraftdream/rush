import { bind } from "@react-carburetor/core";
import type { WSMessage } from "../../types";
import { ws } from "../../ws";

/** Where a system-prompt fetch reports its outcome. */
export interface ISystemPromptSink {
  /** The reply carrying this fetch's content arrived. */
  loaded(content: string): void;
  /** The fetch failed: error reply, dropped connection or timeout. */
  failed(message: string): void;
}

/** Bounded wait for the reply. */
const FETCH_TIMEOUT_MS = 10_000;

/**
 * One correlated get_system_prompt round trip.
 *
 * Correlates the fetch like the save path (task #683): only the reply carrying THIS id may
 * populate the editor, and it must echo the session we asked about. A dropped connection dooms
 * the in-flight fetch, so it is failed explicitly instead of leaving the modal hanging on
 * "Loading…" until the timer fires.
 */
export class SystemPromptFetch {
  private readonly msgID = crypto.randomUUID();

  private settled = false;

  private timer: ReturnType<typeof setTimeout> | undefined;

  private unsubReply: (() => void) | undefined;

  private unsubDisc: (() => void) | undefined;

  private readonly sessionID: string;

  private readonly sink: ISystemPromptSink;

  /**
   * Prepares a fetch; nothing is sent until start().
   *
   * @param sessionID - session whose prompt is wanted
   * @param sink - receives the outcome
   */
  public constructor(sessionID: string, sink: ISystemPromptSink) {
    this.sessionID = sessionID;
    this.sink = sink;
  }

  /** Registers the listeners and the timeout, then sends the request. */
  public start(): void {
    this.timer = setTimeout(this.onTimeout, FETCH_TIMEOUT_MS);
    this.unsubReply = ws.on("*", this.onReply);
    this.unsubDisc = ws.on("_disconnected", this.onDisconnected);
    ws.send("get_system_prompt", { sessionID: this.sessionID }, this.msgID);
  }

  /** Cancels the fetch silently: no outcome is reported after this. */
  public dispose(): void {
    this.settled = true;
    clearTimeout(this.timer);
    this.unsubReply?.();
    this.unsubDisc?.();
  }

  /** Settles the fetch once; false when it was already settled. */
  protected finish(): boolean {
    if (this.settled) return false;
    this.settled = true;
    clearTimeout(this.timer);
    this.unsubReply?.();
    this.unsubDisc?.();
    return true;
  }

  /** The wait ran out. */
  @bind
  protected onTimeout(): void {
    if (!this.finish()) return;
    this.sink.failed("Timed out waiting for the system prompt");
  }

  /**
   * Handles a frame; only the reply carrying this fetch's id counts.
   *
   * @param msg - any incoming frame
   */
  @bind
  protected onReply(msg: WSMessage): void {
    if (msg.id !== this.msgID) return;
    const p = msg.payload as { sessionID?: string; content?: string } | undefined;
    // Defense in depth: the server echoes the requested sessionID in
    // the payload; a success reply naming a different session is not
    // ours to apply (error replies carry no payload, so they pass).
    if (!msg.error && p?.sessionID !== this.sessionID) return;
    if (!this.finish()) return;
    if (msg.error) {
      this.sink.failed(msg.error as string);
    } else {
      this.sink.loaded(p?.content ?? "");
    }
  }

  /** The connection dropped mid-fetch. */
  @bind
  protected onDisconnected(): void {
    if (!this.finish()) return;
    this.sink.failed("Connection lost while loading the system prompt");
  }
}
