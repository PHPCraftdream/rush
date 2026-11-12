import type { WSMessage } from "../types";
import { connectionCarburetor } from "../state/connection";

type Handler = (msg: WSMessage) => void;

/**
 * Safety cap for the offline outbox: beyond this, frames are refused
 * (sendQueued returns false) rather than growing without bound during a long outage.
 */
const OUTBOX_LIMIT = 100;

/** The browser's WebSocket link to the server: reconnect loop, typed handlers and an offline outbox. */
export class WSClient {
  private socket: WebSocket | null = null;
  private handlers = new Map<string, Set<Handler>>();
  private reconnectDelay = 1000;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private closed = false;
  /** Frames accepted by sendQueued while the socket was down, waiting for the next connect (FIFO). */
  private outbox: WSMessage[] = [];

  /** Opens the socket and keeps reconnecting until disconnect() or disableReconnect(). */
  public connect(): void {
    this.closed = false;
    this.dial();
  }

  /** Opens one socket and wires its events to the handler table. */
  private dial(): void {
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
          this.dial();
        }, this.reconnectDelay);
      }
    };

    sock.onerror = () => {
      sock.close();
    };
  }

  /** Closes the socket and cancels any pending reconnect. */
  public disconnect(): void {
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

  /**
   * Stops all future reconnection attempts WITHOUT touching the current socket (task #714).
   *
   * Used after the server confirms a shutdown_server request: the endpoint is
   * intentionally going away, so the auto-reconnect loop would retry forever.
   * The live socket and its handlers stay untouched: final server frames
   * still arrive, and when the connection closes the normal _disconnected
   * handling runs (accurate connected state, keep-alive teardown). The
   * `if (!this.closed)` guard in onclose keeps a reconnect from being scheduled.
   */
  public disableReconnect(): void {
    this.closed = true;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  /**
   * Sends a frame now; true iff it was actually written to the socket.
   *
   * False when the socket is missing/closed, or it flipped out of OPEN between
   * the check and the write. Callers that own user-visible state must not treat
   * a false as "delivered".
   *
   * @param type - frame type
   * @param payload - frame payload
   * @param id - correlation id
   */
  public send<T>(type: string, payload?: T, id?: string): boolean {
    const sock = this.socket;
    if (!sock || sock.readyState !== WebSocket.OPEN) return false;
    const msg: WSMessage<T> = { type, payload, id };
    try {
      sock.send(JSON.stringify(msg));
      return true;
    } catch {
      // Browsers throw InvalidStateError if the socket closed between the
      // readyState check above and this call; report the frame as not sent
      // so callers can queue it or keep their draft.
      return false;
    }
  }

  /** True while the underlying socket is open and writable right now. */
  public isOpen(): boolean {
    return !!this.socket && this.socket.readyState === WebSocket.OPEN;
  }

  /**
   * send() with an offline outbox: a frame is written now, or parked and flushed on the next connect.
   *
   * Returns true when the frame was written now OR parked for delivery; false
   * only when the outbox is full, in which case the frame is dropped and
   * callers should keep any user-visible draft state.
   *
   * @param type - frame type
   * @param payload - frame payload
   * @param id - correlation id
   */
  public sendQueued<T>(type: string, payload?: T, id?: string): boolean {
    if (this.send(type, payload, id)) return true;
    if (this.outbox.length >= OUTBOX_LIMIT) return false;
    this.outbox.push({ type, payload, id });
    connectionCarburetor.setOutboxCount(this.outbox.length);
    return true;
  }

  /** Writes parked frames in order until the outbox is empty or the socket drops again. */
  private flushOutbox(): void {
    while (this.outbox.length > 0) {
      const next = this.outbox[0];
      if (!this.send(next.type, next.payload, next.id)) {
        // dropped again mid-flush; reflect what is still parked
        connectionCarburetor.setOutboxCount(this.outbox.length);
        return;
      }
      this.outbox.shift();
      connectionCarburetor.setOutboxCount(this.outbox.length);
    }
  }

  /**
   * Registers a handler for a frame type ("*" for all, "_connected"/"_disconnected" for link events).
   *
   * @param type - frame type
   * @param handler - called with each matching frame
   * @returns remover for the handler
   */
  public on(type: string, handler: Handler): () => void {
    if (!this.handlers.has(type)) this.handlers.set(type, new Set());
    this.handlers.get(type)!.add(handler);
    return () => this.handlers.get(type)?.delete(handler);
  }

  /**
   * Calls the handlers registered for a type.
   *
   * @param type - frame type
   * @param msg - the frame
   */
  private emit(type: string, msg: WSMessage): void {
    this.handlers.get(type)?.forEach((h) => h(msg));
  }
}
