import { Carburetor } from "@react-carburetor/core";
import { drainSendableQueue } from "../queueOnIdle/drainSendableQueue.ts";
import type { TReadonly } from "@react-carburetor/core";

/** The wire shape of an attachment, shared by the queue and the send wrappers. */
export interface WireAttachment {
  fileName: string;
  mimeType: string;
  /** Base64. */
  data: string;
}

/** One message parked client-side while the session's agent is busy. */
export interface QueuedMessage {
  id: string;
  content: string;
  /** Attachments captured from the composer; they ride on the single flushed send. */
  attachments?: WireAttachment[];
  /** Keeps unsafe-to-resend messages out of automatic busy=false flushes. */
  failureState?: "rejected" | "ambiguous";
  error?: string;
}

/** What a busy=false flush sends: queued texts joined, attachments in queue order. */
export interface FlushedQueue {
  content: string;
  attachments: WireAttachment[];
}

/** Client-side message queue per session. */
export interface IQueueState {
  messageQueue: Record<string, QueuedMessage[]>;
}

/**
 * Replaces the queue table in the draft.
 *
 * @param draft - state to change
 * @param next - the new table
 */
const writeQueue = (draft: IQueueState, next: Record<string, QueuedMessage[]>): void => {
  draft.messageQueue = next;
};

/**
 * Text of a queued message.
 *
 * @param m - the queued message
 */
const contentOf = (m: QueuedMessage): string => m.content;

/**
 * Attachments of a queued message, or none.
 *
 * @param m - the queued message
 */
const attachmentsOf = (m: QueuedMessage): WireAttachment[] => m.attachments ?? [];

/**
 * Copies a queue table without one session entry.
 *
 * @param table - the queue table
 * @param sessionID - session to leave out
 */
const withoutSession = (
  table: Readonly<Record<string, QueuedMessage[]>>,
  sessionID: string,
): Record<string, QueuedMessage[]> => {
  const { [sessionID]: _dropped, ...rest } = table;
  return rest;
};

/** Holds messages the user queued while the agent was busy. */
export class QueueCarburetor extends Carburetor<IQueueState> {
  private idCounter = 0;

  /**
   * Appends a message to the session's queue.
   *
   * @param sessionID - session id
   * @param content - message text
   * @param attachments - attachments captured with it
   */
  public enqueue(sessionID: string, content: string, attachments?: WireAttachment[]): void {
    const table = this.getData().messageQueue;
    this.idCounter += 1;
    this.update(writeQueue, {
      ...table,
      [sessionID]: [...(table[sessionID] ?? []), { id: `q-${this.idCounter}`, content, attachments }],
    });
  }

  /**
   * Drains only messages that have not already failed a per-item send.
   *
   * Rejected and ambiguous items remain queued for an explicit user retry,
   * avoiding both silent loss and accidental duplicates when an agent_busy=false
   * event lands.
   *
   * @param sessionID - session id
   */
  public dequeueAll(sessionID: string): FlushedQueue | undefined {
    const table = this.getData().messageQueue;
    const msgs = table[sessionID] ?? [];
    if (!msgs.length) return undefined;
    const { sendable, retained } = drainSendableQueue(msgs);
    this.update(writeQueue, retained.length ? { ...table, [sessionID]: retained } : withoutSession(table, sessionID));
    if (!sendable.length) return undefined;
    return {
      content: sendable.map(contentOf).join("\n\n"),
      attachments: sendable.flatMap(attachmentsOf),
    };
  }

  /**
   * Removes one queued message.
   *
   * @param sessionID - session id
   * @param id - queued message id
   */
  public remove(sessionID: string, id: string): void {
    const table = this.getData().messageQueue;
    const msgs = (table[sessionID] ?? []).filter((m) => m.id !== id);
    this.update(writeQueue, msgs.length ? { ...table, [sessionID]: msgs } : withoutSession(table, sessionID));
  }

  /**
   * Edits a queued message's text. Clears the displayed error but keeps failureState,
   * so the message still requires an explicit send/retry action.
   *
   * @param sessionID - session id
   * @param id - queued message id
   * @param content - the new text
   */
  public updateContent(sessionID: string, id: string, content: string): void {
    const table = this.getData().messageQueue;
    const msgs = (table[sessionID] ?? []).map((m) => (m.id === id ? { ...m, content, error: undefined } : m));
    this.update(writeQueue, { ...table, [sessionID]: msgs });
  }

  /**
   * Removes one message and returns it with its original index, or undefined if it is gone.
   *
   * @param sessionID - session id
   * @param id - queued message id
   */
  public take(sessionID: string, id: string): { item: QueuedMessage; index: number } | undefined {
    const table = this.getData().messageQueue;
    const msgs = table[sessionID] ?? [];
    const index = msgs.findIndex((m) => m.id === id);
    if (index === -1) return undefined;
    const next = msgs.filter((m) => m.id !== id);
    this.update(writeQueue, next.length ? { ...table, [sessionID]: next } : withoutSession(table, sessionID));
    return { item: msgs[index], index };
  }

  /**
   * Restores a taken message after a failed send, attaching the error for the queue UI.
   *
   * The index is clamped to the current length in case the queue changed size
   * while the send was in flight.
   *
   * @param sessionID - session id
   * @param item - the taken message
   * @param index - its original position
   * @param error - UI-ready failure text
   * @param state - whether the send definitely failed or may have gone out
   */
  public restoreItem(
    sessionID: string,
    item: QueuedMessage,
    index: number,
    error: string,
    state: "rejected" | "ambiguous",
  ): void {
    const table = this.getData().messageQueue;
    const msgs = [...(table[sessionID] ?? [])];
    msgs.splice(Math.min(index, msgs.length), 0, { ...item, error, failureState: state });
    this.update(writeQueue, { ...table, [sessionID]: msgs });
  }

  /**
   * Drops a session's whole queue; true when there was one.
   *
   * @param sessionID - session id
   */
  public removeSession(sessionID: string): boolean {
    const table = this.getData().messageQueue;
    if (!this.hasQueued(sessionID)) return false;
    this.update(writeQueue, withoutSession(table, sessionID));
    return true;
  }

  /** The sessions that currently have queued messages. */
  public queuedSessionIDs(): string[] {
    return Object.keys(this.getData().messageQueue);
  }

  /**
   * Whether the session has queued messages.
   *
   * @param sessionID - session id
   */
  public hasQueued(sessionID: string): boolean {
    return Object.prototype.hasOwnProperty.call(this.getData().messageQueue, sessionID);
  }

  /**
   * The queue table as a Map, for readers that want Map semantics (the conversion edge).
   *
   * @param state - the queue state
   */
  public static asMap(state: TReadonly<IQueueState>): Map<string, QueuedMessage[]> {
    return new Map(Object.entries(state.messageQueue)) as Map<string, QueuedMessage[]>;
  }
}
