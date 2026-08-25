import { atom } from "nanostores";
import type { WSMessage, Session } from "./types";

type Handler = (msg: WSMessage) => void;

// Number of frames parked in the offline outbox (see sendQueued). Exposed
// as a store so UI components can render "N queued" without polling.
export const $wsOutboxCount = atom(0);

// Safety cap for the offline outbox — beyond this, frames are refused
// (sendQueued returns false) rather than growing without bound during a
// long outage.
const OUTBOX_LIMIT = 100;

class WSClient {
  private socket: WebSocket | null = null;
  private handlers = new Map<string, Set<Handler>>();
  private reconnectDelay = 1000;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private closed = false;
  // Frames accepted by sendQueued while the socket was down, waiting for
  // the next successful (re)connect to flush (FIFO).
  private outbox: WSMessage[] = [];

  connect() {
    this.closed = false;
    this._connect();
  }

  private _connect() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/ws`;
    const sock = new WebSocket(url);
    this.socket = sock;

    sock.onopen = () => {
      this.reconnectDelay = 1000;
      // User-authored frames parked while offline go out BEFORE the
      // housekeeping traffic the _connected listeners generate.
      this.flushOutbox();
      this.emit("_connected", { type: "_connected" });
    };

    sock.onmessage = (ev: MessageEvent<string>) => {
      try {
        const msg: WSMessage = JSON.parse(ev.data);
        this.emit(msg.type, msg);
        this.emit("*", msg);
      } catch {
        // ignore malformed
      }
    };

    sock.onclose = () => {
      if (this.socket !== sock) return; // a superseded socket must not touch client state
      this.socket = null;
      this.emit("_disconnected", { type: "_disconnected" });
      if (!this.closed) {
        this.reconnectTimer = setTimeout(() => {
          this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000);
          this._connect();
        }, this.reconnectDelay);
      }
    };

    sock.onerror = () => {
      sock.close();
    };
  }

  disconnect() {
    this.closed = true;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.socket) {
      this.socket.onclose = null; // close() is async; don't let the stale handler fire
      this.socket.close();
    }
    this.socket = null;
  }

  /** Stops all future reconnection attempts WITHOUT touching the current
   * socket (task #714): used after the server confirms a shutdown_server
   * request — the endpoint is intentionally going away, so the normal
   * auto-reconnect loop would retry forever against a server that is
   * deliberately gone. The live socket and its handlers are left untouched: final server frames still arrive, and when the connection eventually closes the normal _disconnected handling runs (accurate $connected state, keep-alive teardown) — the `if (!this.closed)` guard in onclose is what keeps a reconnect from being scheduled. */
  disableReconnect() {
    this.closed = true;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  /** Sends a frame now. Returns true iff the frame was actually written to
   * the socket — false when the socket is missing/closed, or it flipped out
   * of OPEN between the check and the write. Callers that own user-visible
   * state must not treat a false as "delivered". */
  send<T>(type: string, payload?: T, id?: string): boolean {
    const sock = this.socket;
    if (!sock || sock.readyState !== WebSocket.OPEN) return false;
    const msg: WSMessage<T> = { type, payload, id };
    try {
      sock.send(JSON.stringify(msg));
      return true;
    } catch {
      // Browsers throw InvalidStateError if the socket closed between the
      // readyState check above and this call — report the frame as not
      // sent so callers can queue it or keep their draft.
      return false;
    }
  }

  /** True while the underlying socket is open and writable right now. */
  isOpen(): boolean {
    return !!this.socket && this.socket.readyState === WebSocket.OPEN;
  }

  /** send() with an offline outbox: when the socket is down the frame is
   * parked in a FIFO buffer and flushed on the next successful (re)connect.
   * Returns true when the frame was written now OR parked for delivery;
   * false only when the outbox is full, in which case the frame is dropped
   * and callers should keep any user-visible draft state. */
  sendQueued<T>(type: string, payload?: T, id?: string): boolean {
    if (this.send(type, payload, id)) return true;
    if (this.outbox.length >= OUTBOX_LIMIT) return false;
    this.outbox.push({ type, payload, id });
    $wsOutboxCount.set(this.outbox.length);
    return true;
  }

  private flushOutbox() {
    while (this.outbox.length > 0) {
      const next = this.outbox[0];
      if (!this.send(next.type, next.payload, next.id)) {
        $wsOutboxCount.set(this.outbox.length); // socket dropped again mid-flush; reflect what's still parked
        return;
      }
      this.outbox.shift();
      $wsOutboxCount.set(this.outbox.length);
    }
  }

  on(type: string, handler: Handler): () => void {
    if (!this.handlers.has(type)) this.handlers.set(type, new Set());
    this.handlers.get(type)!.add(handler);
    return () => this.handlers.get(type)?.delete(handler);
  }

  private emit(type: string, msg: WSMessage) {
    this.handlers.get(type)?.forEach((h) => h(msg));
  }
}

export const ws = new WSClient();

// ── correlated request/reply (task #725) ────────────────────────────────────
//
// send() returning false means the frame never left the browser, yet
// several UI owners of busy/loading state used to register a reply
// listener and wait forever for a reply that could never arrive — a
// disconnect mid-request left the modal stuck busy permanently (review
// 2026-08-25, P1/P2). This helper bundles the shape the system-prompt
// fetch path already uses — correlation id, bounded timeout, disconnect
// fail-fast — into one reusable primitive: it sends the frame, resolves
// with the first reply carrying the same id, and rejects (detaching
// every listener it registered) on an error reply, a dropped
// connection, a send that could not happen, or the timeout.

export interface WSRequestOptions {
  /** Bounded wait for the correlated reply. */
  timeoutMs?: number;
}

// 10s, matching the system-prompt fetch path's timeout in
// ChatToolbar.tsx — every migrated request is a fast config/DB/filesystem
// round-trip; anything slower than this is treated as lost.
const WS_REQUEST_TIMEOUT_MS = 10_000;

export function wsRequest<T = unknown>(
  type: string,
  payload?: unknown,
  opts: WSRequestOptions = {},
): Promise<WSMessage<T>> {
  return new Promise<WSMessage<T>>((resolve, reject) => {
    const msgID = crypto.randomUUID();
    let timer: ReturnType<typeof setTimeout> | null = null;
    let unsubReply: () => void = () => {};
    let unsubDisc: () => void = () => {};

    function cleanup() {
      if (timer !== null) clearTimeout(timer);
      unsubReply();
      unsubDisc();
    }

    timer = setTimeout(() => {
      cleanup();
      reject(new Error(`Timed out waiting for the ${type} reply`));
    }, opts.timeoutMs ?? WS_REQUEST_TIMEOUT_MS);

    unsubReply = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      cleanup();
      if (msg.error) reject(new Error(msg.error));
      else resolve(msg as WSMessage<T>);
    });

    // A dropped connection dooms the in-flight request — its reply can
    // never arrive on this socket. Fail fast instead of burning the
    // timeout (mirrors the _disconnected handling of the system-prompt
    // fetch path).
    unsubDisc = ws.on("_disconnected", () => {
      cleanup();
      reject(new Error(`Connection lost while waiting for the ${type} reply`));
    });

    // send() reports false when the socket is already down/closed — the
    // same doom as a drop, known before the frame even left. Nothing
    // will ever call the listeners above, so reject now and detach them.
    if (!ws.send(type, payload, msgID)) {
      cleanup();
      reject(new Error(`Not connected to the server — ${type} request was not sent`));
    }
  });
}

// ── live-event epoch (task #689) ────────────────────────────────────────────
//
// The request-ordering guard above cannot see live push events
// (message_created/message_updated/message_deleted) that the client
// applies BETWEEN a load_messages request being sent and its reply
// arriving. A reply that is still the latest REQUEST can carry a DB
// snapshot read BEFORE those events were written — applying it wholesale
// would erase the fresher live state (a just-created message vanishing,
// streamed content reverting, a deleted message resurrecting). Each
// applied live push bumps a per-session epoch; sendLoadMessages records
// the epoch at send time; the messages_list handler compares the two and
// merges instead of replacing when they differ.
const liveEventEpoch = new Map<string, number>();
const requestEpoch = new Map<string, number>();

/** Records that a live push event was applied for sessionID. Called by
 * useWS.ts only at sites where the event actually mutated store state. */
export function bumpLiveEventEpoch(sessionID: string) {
  liveEventEpoch.set(sessionID, (liveEventEpoch.get(sessionID) ?? 0) + 1);
}

/** True if any live push was applied for sessionID since its latest
 * load_messages request was sent. Sessions with no tracked request (and
 * the untagged back-compat reply path) report false. */
export function hasLiveEventsSinceRequest(sessionID: string | undefined): boolean {
  if (!sessionID) return false;
  return (liveEventEpoch.get(sessionID) ?? 0) > (requestEpoch.get(sessionID) ?? 0);
}

// ── delete high-water mark (task #731) ──────────────────────────────────────
//
// The epoch counter above proves "did any live push land since I sent this
// request" — a statement about EVENT COUNT, tied to wall-clock send/apply
// timing. It says nothing about whether THIS SPECIFIC reply's underlying DB
// read actually postdates a delete the client already knows about. Two real
// (not just theoretical-reordering) mechanisms can desync those:
//
//   - the server dispatches load_messages across a worker pool with no FIFO
//     guarantee AND serves reads off a SEPARATE read-only connection pool
//     (internal/db.ConnectRead) for concurrency with the writer — a read
//     transaction's SQLite MVCC snapshot can be pinned before a delete's
//     write commits even when the REQUEST was sent after the delete's push
//     already arrived, with no ordering violation visible at the
//     message/counter level;
//   - message_deleted is exactly one WS frame; a socket hiccup that reorders
//     or (per the epoch mechanism's own known limit) drops it entirely
//     leaves no local signal that anything happened at all.
//
// Fix: every message_deleted push and every messages_list reply now carries
// a monotonic watermark — the deleted row's SQLite rowid, and the session's
// max rowid as of the snapshot read, respectively (see
// internal/message/content.go's Message.RowID doc comment for the full
// server-side mechanism). A snapshot reply whose watermark is LOWER than the
// high-water mark recorded from a delete this client already applied is
// PROVABLY older than that delete, regardless of what the epoch counter
// says — mergeMessageLists must keep filtering that message's tombstone
// even on an otherwise "epoch-clean" reply, and applyMessagesSnapshot must
// not clear the tombstone set for it.
//
// This is strictly additive to the epoch/tombstone mechanism, not a
// replacement: sessions whose deletes carry no watermark (RowID missing —
// e.g. a lookup failure on the fork's best-effort GetMessageRowID call, or a
// stale cached frontend talking to a pre-watermark server) fall back to the
// existing epoch heuristic exactly as before, since deleteHighWaterMark
// simply never advances past 0 for them and every real snapshot watermark
// is >= 0.
const deleteHighWaterMark = new Map<string, number>();

/** Records a delete watermark for sessionID from a message_deleted push
 * actually applied. Never lowers the mark — deletes can be recorded out of
 * order relative to each other (independent messages, no ordering
 * guarantee between distinct deletes), and only the highest one seen so far
 * is a valid floor for judging a snapshot's freshness. rowID <= 0 (missing/
 * unavailable) is a no-op: it must not regress the mark to something that
 * would compare as "older" than the true high-water mark and weaken
 * protection for an already-recorded delete. */
export function bumpDeleteHighWaterMark(sessionID: string, rowID: number | undefined) {
  if (!rowID || rowID <= 0) return;
  const prev = deleteHighWaterMark.get(sessionID) ?? 0;
  if (rowID > prev) deleteHighWaterMark.set(sessionID, rowID);
}

/** True if sessionID's recorded delete high-water mark is STRICTLY newer
 * than the given snapshot watermark — i.e. this snapshot's DB read is
 * provably older than a delete already applied for this session, so the
 * caller must not trust it to clear tombstones (and must keep merging
 * rather than wholesale-replacing) even if the epoch heuristic alone would
 * call the reply clean. snapshotWatermark undefined (untagged/back-compat
 * reply, or a server that predates this fix) always reports false — the
 * existing epoch heuristic is the only signal available for it, unchanged. */
export function isSnapshotStaleForDeletes(sessionID: string | undefined, snapshotWatermark: number | undefined): boolean {
  if (!sessionID || snapshotWatermark === undefined) return false;
  return (deleteHighWaterMark.get(sessionID) ?? 0) > snapshotWatermark;
}

// ── load_messages stale-reply guard (task #685) ────────────────────────────
//
// Several independent call sites (the two OwnedExternal pollers in
// useWS.ts, hashchange/session_created/sessions_list routing, Sidebar's
// session switch, SubAgentBlock's lazy load) each fire their own
// `load_messages` for a session with no coordination between them. The
// server dispatches load_messages through a per-connection worker pool
// with no FIFO guarantee (hub.go's workerPoolSize goroutines draining one
// shared queue), so two in-flight requests for the same session can have
// their `messages_list` replies arrive in EITHER order — a reply to a
// request sent earlier can land after the reply to one sent later,
// carrying a stale (pre-update) snapshot that would silently regress the
// rendered transcript if applied unconditionally.
//
// Fix: every load_messages send is tagged with a msgID and recorded here
// as the latest SENT request for that session — this entry is intentionally
// never cleared (not even once its reply is accepted): it must keep acting
// as a floor so that a slower, older reply arriving even after the newer
// one was already applied is still recognized and dropped, not just while
// something is nominally "in flight". A messages_list reply is applied only
// if its `id` matches the latest recorded request for its session (or
// there's nothing recorded, e.g. an older cached frontend that never sent
// an id at all) — replies to any superseded request are dropped.
// Entries live until the session is removed from the store — the one end
// that doesn't weaken the floor — via forgetSessionRequestState below. See
// sendLoadMessages/isStaleMessagesReply below.
const latestLoadMessagesID = new Map<string, string>();

/** Sends load_messages for sessionID, tagged with a fresh msgID that
 * becomes the only one whose reply will be accepted for this session
 * until a newer load_messages supersedes it. */
export function sendLoadMessages(sessionID: string) {
  const id = crypto.randomUUID();
  latestLoadMessagesID.set(sessionID, id);
  requestEpoch.set(sessionID, liveEventEpoch.get(sessionID) ?? 0);
  ws.send("load_messages", { sessionID }, id);
}

/** True if a messages_list reply for sessionID/replyID is stale — i.e. it
 * is not the answer to the most recently sent load_messages for this
 * session (a newer request has since superseded it, whether or not that
 * newer request's reply has arrived yet). Replies with no id (or for a
 * session with no tracked request at all) are never considered stale, so
 * back-compat/untagged paths keep working. */
export function isStaleMessagesReply(sessionID: string | undefined, replyID: string | undefined): boolean {
  if (!sessionID || !replyID) return false;
  const latest = latestLoadMessagesID.get(sessionID);
  if (!latest) return false;
  return latest !== replyID;
}

/** Drops every per-session request-bookkeeping entry (stale-reply floor,
 * live-event epochs, delete high-water mark) for a session that has been
 * removed from $sessions. Session IDs are never reused, so nothing arriving
 * later for this ID needs the tracking; without this, all four maps above
 * grow one entry per session ever loaded for the page's lifetime. Called
 * from store.ts removeSession, the single choke point every session-removal
 * path funnels through (session_deleted, sidebar delete replies). */
export function forgetSessionRequestState(sessionID: string) {
  latestLoadMessagesID.delete(sessionID);
  liveEventEpoch.delete(sessionID);
  requestEpoch.delete(sessionID);
  deleteHighWaterMark.delete(sessionID);
}

// ── sessions_list live-race guard (task #690) ───────────────────────────────
//
// list_sessions is polled every 5s plus on reconnect/tab-focus, and the server
// dispatches it through the same non-FIFO worker pool that bit messages_list
// (#685): a reply to an older request can land after a newer one's. Worse,
// even the latest request's reply can carry a DB snapshot read BEFORE live
// session_created/updated/deleted pushes were applied client-side — a wholesale
// setSessions then resurrects a deleted session, drops a created one, or reverts
// a title. Mirrors #685 (correlation ID drop) + #689 (live-event reconciliation),
// simplified: sessions are a flat ID-keyed list with no streaming/partial content,
// so a straight replay of live deltas recorded since the request was sent is sufficient.
interface sessionsLiveRecord {
  seq: number;
  session?: Session;
  deleted?: boolean;
}
const sessionsLiveRecords = new Map<string, sessionsLiveRecord>();
let sessionsLiveSeq = 0;
let sessionsRequestSeq = 0;
let latestListSessionsID: string | undefined;

/** Records a live session_created/session_updated payload (task #690). */
export function recordSessionLiveUpdate(s: Session) {
  sessionsLiveRecords.set(s.ID, { seq: ++sessionsLiveSeq, session: s });
}

/** Records a live session_deleted push (task #690). */
export function recordSessionLiveDelete(id: string) {
  sessionsLiveRecords.set(id, { seq: ++sessionsLiveSeq, deleted: true });
}

/** Sends list_sessions tagged with a fresh correlation ID that becomes the
 * only one whose reply will be accepted (mirrors sendLoadMessages). Also
 * records the live-event watermark at send time. Returns whether the
 * frame actually left the socket (see WSClient.send) so reconnect
 * callers can report a drop instead of silently ignoring it (task #727);
 * the pollers re-issue it every 5s regardless. */
export function sendListSessions(): boolean {
  const id = crypto.randomUUID();
  latestListSessionsID = id;
  sessionsRequestSeq = sessionsLiveSeq;
  return ws.send("list_sessions", undefined, id);
}

/** True if a sessions_list reply is not the answer to the most recently sent
 * list_sessions (mirrors isStaleMessagesReply; untagged/unknown → false). */
export function isStaleSessionsListReply(replyID: string | undefined): boolean {
  if (!replyID || !latestListSessionsID) return false;
  return latestListSessionsID !== replyID;
}

/** Collects the live deltas recorded since the latest request was sent and
 * clears the log. Called only when a fresh tagged reply is applied: by then
 * every retained record (seq <= request watermark) is already reflected in
 * the snapshot read, and the seq > watermark ones are being replayed now, so
 * the whole log is safe to drop. Untagged back-compat replies must NOT call
 * this — clearing on them would strip protection from a tagged request that
 * is still in flight. */
export function takeSessionsLiveDelta(): { upserts: Session[]; deletedIDs: string[] } {
  const upserts: Session[] = [];
  const deletedIDs: string[] = [];
  for (const [id, rec] of sessionsLiveRecords) {
    if (rec.seq <= sessionsRequestSeq) continue;
    if (rec.deleted) deletedIDs.push(id);
    else if (rec.session) upserts.push(rec.session);
  }
  sessionsLiveRecords.clear();
  return { upserts, deletedIDs };
}
