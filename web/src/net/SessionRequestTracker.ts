import type { Session } from "../types";

/** A live session push recorded while a sessions_list request is in flight. */
interface ISessionsLiveRecord {
  seq: number;
  session?: Session;
  deleted?: boolean;
}

/**
 * Per-session request bookkeeping that keeps stale or raced server replies from regressing the UI.
 *
 * Four guards live here, each answering "can this reply be trusted?":
 *
 * 1. Live-event epoch (task #689). The request-ordering guard cannot see live
 *    pushes (message_created/updated/deleted) applied BETWEEN a load_messages
 *    request being sent and its reply arriving. Each applied push bumps a
 *    per-session epoch; beginLoadMessages records the epoch at send time; the
 *    messages_list handler merges instead of replacing when they differ.
 *
 * 2. Delete high-water mark (tasks #731/#737). The epoch proves "did any live
 *    push land since I sent this request", not whether THIS reply's DB read
 *    postdates a delete already applied (the server dispatches through a
 *    non-FIFO worker pool and reads off a separate read-only pool, so a read
 *    snapshot can be pinned before a delete commits). Every message_deleted
 *    push and messages_list reply carries a monotonic delete-GENERATION
 *    counter (see internal/message/content.go's Message.DeleteGeneration); a
 *    snapshot whose watermark is LOWER than the recorded mark is provably older
 *    than that delete. Strictly additive: servers without the field fall back
 *    to the epoch heuristic, since the mark then never advances past 0.
 *
 * 3. load_messages stale-reply guard (task #685). Several uncoordinated call
 *    sites fire load_messages; replies can arrive in either order. Every send
 *    is tagged with an id recorded as the latest for its session. The entry is
 *    intentionally never cleared on reply: it must keep acting as a floor so a
 *    slower, older reply is still dropped. Entries live until the session is
 *    removed (forgetSession).
 *
 * 4. sessions_list live-race guard (task #690). list_sessions is polled every
 *    5s and can race live session pushes the same way; live deltas recorded
 *    since the request was sent are replayed over the snapshot.
 */
export class SessionRequestTracker {
  private readonly liveEventEpoch = new Map<string, number>();
  private readonly requestEpoch = new Map<string, number>();
  private readonly deleteHighWaterMark = new Map<string, number>();
  private readonly latestLoadMessagesID = new Map<string, string>();
  private readonly sessionsLiveRecords = new Map<string, ISessionsLiveRecord>();
  private sessionsLiveSeq = 0;
  private sessionsRequestSeq = 0;
  private latestListSessionsID: string | undefined;

  /**
   * Records that a live push event was applied for a session.
   *
   * Called only at sites where the event actually mutated store state.
   *
   * @param sessionID - session id
   */
  public bumpLiveEventEpoch(sessionID: string): void {
    this.liveEventEpoch.set(sessionID, (this.liveEventEpoch.get(sessionID) ?? 0) + 1);
  }

  /**
   * True if any live push was applied since the session's latest load_messages request was sent.
   *
   * Sessions with no tracked request (and the untagged back-compat reply path) report false.
   *
   * @param sessionID - session id
   */
  public hasLiveEventsSinceRequest(sessionID: string | undefined): boolean {
    if (!sessionID) return false;
    return (this.liveEventEpoch.get(sessionID) ?? 0) > (this.requestEpoch.get(sessionID) ?? 0);
  }

  /**
   * Records a delete watermark from a message_deleted push that was actually applied.
   *
   * Never lowers the mark: deletes can be recorded out of order, and only the
   * highest seen so far is a valid floor. generation <= 0 (missing, e.g. a
   * pre-#737 server) is a no-op so it cannot weaken protection already recorded.
   *
   * @param sessionID - session id
   * @param generation - the push's DeleteGeneration
   */
  public bumpDeleteHighWaterMark(sessionID: string, generation: number | undefined): void {
    if (!generation || generation <= 0) return;
    const prev = this.deleteHighWaterMark.get(sessionID) ?? 0;
    if (generation > prev) this.deleteHighWaterMark.set(sessionID, generation);
  }

  /**
   * True if the recorded delete mark is STRICTLY newer than the snapshot's watermark.
   *
   * Then the snapshot's DB read is provably older than a delete already applied,
   * so the caller must keep merging rather than wholesale-replacing even if
   * the epoch heuristic would call the reply clean. An undefined watermark
   * (untagged reply, or a server predating the fix) always reports false.
   *
   * @param sessionID - session id
   * @param snapshotWatermark - the reply's watermark
   */
  public isSnapshotStaleForDeletes(sessionID: string | undefined, snapshotWatermark: number | undefined): boolean {
    if (!sessionID || snapshotWatermark === undefined) return false;
    return (this.deleteHighWaterMark.get(sessionID) ?? 0) > snapshotWatermark;
  }

  /**
   * Starts a load_messages request: its fresh id becomes the only one whose reply is accepted.
   *
   * @param sessionID - session id
   * @returns the correlation id to send with the frame
   */
  public beginLoadMessages(sessionID: string): string {
    const id = crypto.randomUUID();
    this.latestLoadMessagesID.set(sessionID, id);
    this.requestEpoch.set(sessionID, this.liveEventEpoch.get(sessionID) ?? 0);
    return id;
  }

  /**
   * True if a messages_list reply is not the answer to the latest load_messages for its session.
   *
   * Replies with no id (or for a session with no tracked request) are never
   * stale, so back-compat/untagged paths keep working.
   *
   * @param sessionID - session id
   * @param replyID - correlation id of the reply
   */
  public isStaleMessagesReply(sessionID: string | undefined, replyID: string | undefined): boolean {
    if (!sessionID || !replyID) return false;
    const latest = this.latestLoadMessagesID.get(sessionID);
    if (!latest) return false;
    return latest !== replyID;
  }

  /**
   * Drops every per-session entry for a session removed from the sessions list.
   *
   * Session IDs are never reused, so without this the maps would grow one
   * entry per session ever loaded for the page's lifetime. Called from the
   * removeSession action, the choke point every removal path funnels through.
   *
   * @param sessionID - session id
   */
  public forgetSession(sessionID: string): void {
    this.latestLoadMessagesID.delete(sessionID);
    this.liveEventEpoch.delete(sessionID);
    this.requestEpoch.delete(sessionID);
    this.deleteHighWaterMark.delete(sessionID);
  }

  /**
   * Records a live session_created/session_updated payload (task #690).
   *
   * @param s - the pushed session
   */
  public recordSessionLiveUpdate(s: Session): void {
    this.sessionsLiveSeq += 1;
    this.sessionsLiveRecords.set(s.ID, { seq: this.sessionsLiveSeq, session: s });
  }

  /**
   * Records a live session_deleted push (task #690).
   *
   * @param id - the deleted session
   */
  public recordSessionLiveDelete(id: string): void {
    this.sessionsLiveSeq += 1;
    this.sessionsLiveRecords.set(id, { seq: this.sessionsLiveSeq, deleted: true });
  }

  /**
   * Starts a list_sessions request: its fresh id becomes the only one whose reply is accepted.
   *
   * Also records the live-event watermark at send time.
   *
   * @returns the correlation id to send with the frame
   */
  public beginListSessions(): string {
    const id = crypto.randomUUID();
    this.latestListSessionsID = id;
    this.sessionsRequestSeq = this.sessionsLiveSeq;
    return id;
  }

  /**
   * True if a sessions_list reply is not the answer to the latest list_sessions (untagged/unknown: false).
   *
   * @param replyID - correlation id of the reply
   */
  public isStaleSessionsListReply(replyID: string | undefined): boolean {
    if (!replyID || !this.latestListSessionsID) return false;
    return this.latestListSessionsID !== replyID;
  }

  /**
   * Collects the live deltas recorded since the latest request was sent and clears the log.
   *
   * Called only when a fresh tagged reply is applied: by then every retained
   * record (seq <= request watermark) is reflected in the snapshot, and the
   * newer ones are replayed now, so the whole log is safe to drop. Untagged
   * back-compat replies must NOT call this: clearing on them would strip
   * protection from a tagged request still in flight.
   */
  public takeSessionsLiveDelta(): { upserts: Session[]; deletedIDs: string[] } {
    const upserts: Session[] = [];
    const deletedIDs: string[] = [];
    for (const [id, rec] of this.sessionsLiveRecords) {
      if (rec.seq <= this.sessionsRequestSeq) continue;
      if (rec.deleted) deletedIDs.push(id);
      else if (rec.session) upserts.push(rec.session);
    }
    this.sessionsLiveRecords.clear();
    return { upserts, deletedIDs };
  }
}
