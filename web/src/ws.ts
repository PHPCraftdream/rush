import { atom } from "nanostores";
import type { WSMessage } from "./types";

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
// an id at all) — replies to any superseded request are dropped. See
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
