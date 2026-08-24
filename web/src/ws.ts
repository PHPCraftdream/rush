import type { WSMessage } from "./types";

type Handler = (msg: WSMessage) => void;

class WSClient {
  private socket: WebSocket | null = null;
  private handlers = new Map<string, Set<Handler>>();
  private reconnectDelay = 1000;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private closed = false;

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

  send<T>(type: string, payload?: T, id?: string) {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) return;
    const msg: WSMessage<T> = { type, payload, id };
    this.socket.send(JSON.stringify(msg));
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
