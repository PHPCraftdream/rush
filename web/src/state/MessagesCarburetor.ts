import { Carburetor } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { Message } from "../types";

/** Merges a pushed assistant row over the live one without letting streamed content regress. */
export type TMergeAssistant = (existing: Message, incoming: Message) => Message;

/**
 * Transcript of the active session plus sub-agent transcripts and block breaks.
 *
 * The keyed tables are plain records (converted to Maps at the read edge), so
 * the engine can track them field by field.
 */
export interface IMessagesState {
  messages: Message[];
  /** Sub-agent session ID to parent session ID. */
  subAgentSessions: Record<string, string>;
  /** Sub-agent session ID to its messages. */
  subAgentMessages: Record<string, Message[]>;
  /** Message ID to the part indices where a new visual block starts (5s gap, zebra pattern). */
  messageBlockBreaks: Record<string, number[]>;
}

/** Gap between part arrivals that starts a new visual block. */
const BLOCK_BREAK_GAP_MS = 5000;

/**
 * Replaces the active transcript in the draft.
 *
 * @param draft - state to change
 * @param messages - the new transcript
 */
const writeMessages = (draft: IMessagesState, messages: Message[]): void => {
  draft.messages = messages;
};

/**
 * Replaces the sub-agent registrations in the draft.
 *
 * @param draft - state to change
 * @param table - the new registrations
 */
const writeSubSessions = (draft: IMessagesState, table: Record<string, string>): void => {
  draft.subAgentSessions = table;
};

/**
 * Replaces the sub-agent transcripts in the draft.
 *
 * @param draft - state to change
 * @param table - the new transcripts
 */
const writeSubMessages = (draft: IMessagesState, table: Record<string, Message[]>): void => {
  draft.subAgentMessages = table;
};

/**
 * Replaces the block-break table in the draft.
 *
 * @param draft - state to change
 * @param table - the new table
 */
const writeBlockBreaks = (draft: IMessagesState, table: Record<string, number[]>): void => {
  draft.messageBlockBreaks = table;
};

/**
 * Writes whichever of the three sub-agent tables changed, in one publication.
 *
 * @param draft - state to change
 * @param subs - new registrations, or null to keep
 * @param subMsgs - new transcripts, or null to keep
 * @param breaks - new block breaks, or null to keep
 */
const writeSubAgentReap = (
  draft: IMessagesState,
  subs: Record<string, string> | null,
  subMsgs: Record<string, Message[]> | null,
  breaks: Record<string, number[]> | null,
): void => {
  if (subs) draft.subAgentSessions = subs;
  if (subMsgs) draft.subAgentMessages = subMsgs;
  if (breaks) draft.messageBlockBreaks = breaks;
};

/**
 * Whether a record has the key as its own.
 *
 * @param table - the record
 * @param key - the key
 */
const hasKey = (table: Readonly<Record<string, unknown>>, key: string): boolean =>
  Object.prototype.hasOwnProperty.call(table, key);

/**
 * Message by id, for lookup tables.
 *
 * @param m - the message
 */
const entryOf = (m: Message): [string, Message] => [m.ID, m];

/**
 * A message id.
 *
 * @param m - the message
 */
const idOf = (m: Message): string => m.ID;

/**
 * Combines a still-latest-but-stale snapshot with the live state that raced it.
 *
 * Pure: reads the tombstone set, writes nothing. Deleted rows are filtered
 * out; an ID in both lists keeps the fresher row (assistant pairs through the
 * merge strategy, other roles by UpdatedAt, a tie keeps the live row); live
 * rows absent from the snapshot are appended after it.
 *
 * @param tomb - IDs deleted by a live push, or undefined
 * @param existing - the live list
 * @param snapshot - the snapshot rows
 * @param mergeAssistant - assistant-pair merge strategy
 */
const mergeMessageLists = (
  tomb: ReadonlySet<string> | undefined,
  existing: readonly Message[],
  snapshot: Message[],
  mergeAssistant: TMergeAssistant,
): Message[] => {
  const snap = tomb ? snapshot.filter((m) => !tomb.has(m.ID)) : snapshot;
  const existingByID = new Map(existing.map(entryOf));
  const merged = snap.map((sm) => {
    const live = existingByID.get(sm.ID);
    if (!live) return sm;
    if (live.Role === "assistant" && sm.Role === "assistant") {
      return mergeAssistant(live, sm);
    }
    return sm.UpdatedAt > live.UpdatedAt ? sm : live;
  });
  const snapIDs = new Set(snap.map(idOf));
  const liveTail = existing.filter((m) => !snapIDs.has(m.ID));
  return [...merged, ...liveTail];
};

/**
 * Holds the active transcript and every sub-agent transcript.
 *
 * Snapshot application follows the messages_list protocol (tasks #689/#731/#737):
 * a reply that raced live pushes is MERGED, an epoch-clean one replaces
 * wholesale so server-side deletes and compaction converge.
 */
export class MessagesCarburetor extends Carburetor<IMessagesState> {
  /** Per-session IDs deleted by a live message_deleted push; cleared by a clean snapshot. */
  private readonly tombstones = new Map<string, Set<string>>();

  /** Last part count and time per message, for block-break detection. */
  private readonly partTracker = new Map<string, { time: number; count: number }>();

  private readonly mergeAssistant: TMergeAssistant;

  /**
   * Creates the store.
   *
   * @param data - initial state
   * @param mergeAssistant - assistant-pair merge strategy
   */
  public constructor(data: IMessagesState, mergeAssistant: TMergeAssistant) {
    super(data);
    this.mergeAssistant = mergeAssistant;
  }

  /**
   * Replaces the active transcript.
   *
   * @param msgs - the new transcript
   */
  public setMessages(msgs: Message[]): void {
    this.update(writeMessages, msgs);
  }

  /**
   * Appends a message or replaces the row with the same id (assistant rows never regress).
   *
   * @param msg - the pushed message
   */
  public upsertMessage(msg: Message): void {
    const list = this.getData().messages;
    const idx = list.findIndex((m) => m.ID === msg.ID);
    if (idx === -1) {
      this.update(writeMessages, [...list, msg]);
      return;
    }
    const prev = list[idx];
    const next = [...list];
    if (prev.Role === "assistant" && msg.Role === "assistant") {
      const merged = this.mergeAssistant(prev, msg);
      if (merged !== msg) {
        console.warn("[upsertMessage] content regression blocked", { id: msg.ID });
      }
      next[idx] = merged;
    } else {
      next[idx] = msg;
    }
    this.update(writeMessages, next);
  }

  /**
   * Removes one message from the active transcript.
   *
   * @param id - message id
   */
  public removeMessage(id: string): void {
    this.update(writeMessages, this.getData().messages.filter((m) => m.ID !== id));
  }

  /**
   * Registers a sub-agent session under its parent.
   *
   * @param subSessionID - the sub-agent session
   * @param parentSessionID - its parent
   */
  public registerSubAgentSession(subSessionID: string, parentSessionID: string): void {
    this.update(writeSubSessions, { ...this.getData().subAgentSessions, [subSessionID]: parentSessionID });
  }

  /**
   * Whether the session is a registered sub-agent session.
   *
   * @param sessionID - session id
   */
  public isSubAgentSession(sessionID: string): boolean {
    return hasKey(this.getData().subAgentSessions, sessionID);
  }

  /**
   * The parent of a sub-agent session.
   *
   * @param subSessionID - sub-agent session id
   */
  public getParentSessionID(subSessionID: string): string | undefined {
    const table = this.getData().subAgentSessions;
    return hasKey(table, subSessionID) ? table[subSessionID] : undefined;
  }

  /**
   * The block-break table as Sets keyed by message id, for readers that want Set semantics (the conversion edge).
   *
   * @param state - the messages state
   */
  public static blockBreakSets(state: TReadonly<IMessagesState>): Map<string, Set<number>> {
    const out = new Map<string, Set<number>>();
    for (const [id, breaks] of Object.entries(state.messageBlockBreaks)) out.set(id, new Set(breaks));
    return out;
  }

  /**
   * The parent sessions of every registered sub-agent session.
   */
  public parentSessionIDs(): Set<string> {
    return new Set(Object.values(this.getData().subAgentSessions));
  }

  /**
   * A sub-agent transcript as stored, or an empty list.
   *
   * @param sessionID - sub-agent session
   */
  private subTranscript(sessionID: string): readonly Message[] {
    const table = this.getData().subAgentMessages;
    return hasKey(table, sessionID) ? table[sessionID] : [];
  }

  /**
   * Inserts or replaces one message in a sub-agent transcript.
   *
   * @param sessionID - sub-agent session
   * @param msg - the pushed message
   */
  public upsertSubAgentMessage(sessionID: string, msg: Message): void {
    const msgs = [...this.subTranscript(sessionID)];
    const idx = msgs.findIndex((m) => m.ID === msg.ID);
    if (idx === -1) {
      msgs.push(msg);
    } else {
      msgs[idx] = msg;
    }
    this.update(writeSubMessages, { ...this.getData().subAgentMessages, [sessionID]: msgs });
  }

  /**
   * Replaces a sub-agent transcript.
   *
   * @param sessionID - sub-agent session
   * @param msgs - the new transcript
   */
  public setSubAgentMessages(sessionID: string, msgs: Message[]): void {
    this.update(writeSubMessages, { ...this.getData().subAgentMessages, [sessionID]: msgs });
  }

  /**
   * Drops one message from a sub-agent transcript.
   *
   * The WS message_deleted handler routes compaction deletions here so a
   * compacted sub-agent block stops rendering messages the backend deleted.
   *
   * @param sessionID - sub-agent session
   * @param messageID - message to drop
   */
  public removeSubAgentMessage(sessionID: string, messageID: string): void {
    if (!hasKey(this.getData().subAgentMessages, sessionID)) return;
    const msgs = this.subTranscript(sessionID);
    const next = msgs.filter((m) => m.ID !== messageID);
    if (next.length === msgs.length) return;
    this.update(writeSubMessages, { ...this.getData().subAgentMessages, [sessionID]: next });
  }

  /**
   * Records that a message was deleted by a live push, so merged snapshots keep filtering it.
   *
   * @param sessionID - session the message belonged to
   * @param messageID - the deleted message
   */
  public tombstone(sessionID: string, messageID: string): void {
    const set = this.tombstones.get(sessionID);
    if (set) set.add(messageID);
    else this.tombstones.set(sessionID, new Set([messageID]));
  }

  /**
   * Applies a messages_list snapshot to the main chat.
   *
   * @param sessionID - session of the reply
   * @param msgs - snapshot rows
   * @param liveRaced - a live push (or a newer delete watermark) outran the snapshot
   */
  public applyMessagesSnapshot(sessionID: string, msgs: Message[], liveRaced: boolean): void {
    if (!liveRaced) {
      this.setMessages(msgs);
      // A clean read postdates any recorded delete, so dropping the set bounds memory.
      this.tombstones.delete(sessionID);
      return;
    }
    const tomb = this.tombstones.get(sessionID);
    this.setMessages(mergeMessageLists(tomb, this.getData().messages, msgs, this.mergeAssistant));
  }

  /**
   * Sub-agent transcript counterpart of applyMessagesSnapshot.
   *
   * @param sessionID - sub-agent session of the reply
   * @param msgs - snapshot rows
   * @param liveRaced - a live push (or a newer delete watermark) outran the snapshot
   */
  public applySubAgentMessagesSnapshot(sessionID: string, msgs: Message[], liveRaced: boolean): void {
    if (!liveRaced) {
      this.setSubAgentMessages(sessionID, msgs);
      this.tombstones.delete(sessionID);
      return;
    }
    const tomb = this.tombstones.get(sessionID);
    const existing = this.subTranscript(sessionID);
    this.setSubAgentMessages(sessionID, mergeMessageLists(tomb, existing, msgs, this.mergeAssistant));
  }

  /**
   * Notes a message's part count and marks a block break when a new part arrives after a 5s gap.
   *
   * @param msgID - message id
   * @param parts - the message's parts
   */
  public trackMessageParts(msgID: string, parts: readonly { type: string }[]): void {
    const count = parts.length;
    const now = Date.now();
    const prev = this.partTracker.get(msgID);

    if (prev && count > prev.count && now - prev.time > BLOCK_BREAK_GAP_MS) {
      const newPart = parts[prev.count];
      if (newPart && newPart.type !== "tool_result" && newPart.type !== "finish") {
        const table = this.getData().messageBlockBreaks;
        const known = hasKey(table, msgID) ? table[msgID] : [];
        const next = known.includes(prev.count) ? known : [...known, prev.count];
        this.update(writeBlockBreaks, { ...table, [msgID]: next });
      }
    }

    this.partTracker.set(msgID, { time: now, count });
  }

  /**
   * Clears every piece of state that outlives a removed session's row (task #727).
   *
   * Drops the sub-agent registration and transcript keyed by the id itself, plus
   * those of every sub-agent session parented by it (one level: sub-agents cannot
   * spawn sub-agents), the delete tombstones, and the message-block breaks that
   * can still be attributed through the message lists.
   *
   * @param id - the removed session
   * @param isActive - whether the removed session is the active one (its transcript is in `messages`)
   */
  public removeSubAgentState(id: string, isActive: boolean): void {
    const data = this.getData();
    const doomed = [id];
    for (const [sub, parent] of Object.entries(data.subAgentSessions)) {
      if (parent === id) doomed.push(sub);
    }

    const msgIDs = new Set<string>();
    if (isActive) {
      for (const m of data.messages) msgIDs.add(m.ID);
    }
    const subs = { ...data.subAgentSessions };
    const subMsgs = { ...data.subAgentMessages };
    for (const d of doomed) {
      delete subs[d];
      for (const m of this.subTranscript(d)) msgIDs.add(m.ID);
      delete subMsgs[d];
      this.tombstones.delete(d);
    }
    let breaks: Record<string, number[]> | null = null;
    if (msgIDs.size > 0) {
      const copy = { ...data.messageBlockBreaks };
      let dropped = false;
      for (const mid of msgIDs) {
        if (hasKey(copy, mid)) {
          delete copy[mid];
          dropped = true;
        }
      }
      if (dropped) breaks = copy;
    }
    this.update(
      writeSubAgentReap,
      Object.keys(subs).length !== Object.keys(data.subAgentSessions).length ? subs : null,
      Object.keys(subMsgs).length !== Object.keys(data.subAgentMessages).length ? subMsgs : null,
      breaks,
    );
  }
}
