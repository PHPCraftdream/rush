import type { WSMessage } from "../types";
import { ws } from "./client";
import { WSRequestError } from "./WSRequestError";

/** Options of a correlated request. */
export interface WSRequestOptions {
  /** Bounded wait for the correlated reply. */
  timeoutMs?: number;
}

/**
 * 10s, matching the system-prompt fetch path's timeout in ChatToolbar.tsx.
 * Every migrated request is a fast config/DB/filesystem round-trip; anything
 * slower than this is treated as lost.
 */
const WS_REQUEST_TIMEOUT_MS = 10_000;

/** Listener detachers of one in-flight request, plus its timeout. */
interface IRequestHandles {
  timer: ReturnType<typeof setTimeout> | null;
  unsubReply: () => void;
  unsubDisc: () => void;
}

/** Detacher of a listener that is not registered yet. */
const noop = (): void => {};

/**
 * Cancels the timeout and detaches both listeners of a request.
 *
 * @param handles - the request's handles
 */
const cleanup = (handles: IRequestHandles): void => {
  if (handles.timer !== null) clearTimeout(handles.timer);
  handles.unsubReply();
  handles.unsubDisc();
};

/**
 * Sends a frame and resolves with the first reply carrying the same id (task #725).
 *
 * Bundles correlation id, bounded timeout and disconnect fail-fast into one
 * primitive: it rejects (detaching every listener it registered) on an error
 * reply, a dropped connection, a send that could not happen, or the timeout, so
 * UI owners of busy/loading state never wait forever for a reply that cannot arrive.
 *
 * @param type - frame type
 * @param payload - frame payload
 * @param opts - request options
 */
export function wsRequest<T = unknown>(
  type: string,
  payload?: unknown,
  opts: WSRequestOptions = {},
): Promise<WSMessage<T>> {
  return new Promise<WSMessage<T>>((resolve, reject) => {
    const msgID = crypto.randomUUID();
    const handles: IRequestHandles = { timer: null, unsubReply: noop, unsubDisc: noop };

    // Ambiguous: the frame was written, so the server may be mid-processing
    // (or already done) when the wait gives up.
    handles.timer = setTimeout(() => {
      cleanup(handles);
      reject(new WSRequestError(`Timed out waiting for the ${type} reply`, true));
    }, opts.timeoutMs ?? WS_REQUEST_TIMEOUT_MS);

    handles.unsubReply = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      cleanup(handles);
      // A definite error reply means the server rejected the request
      // outright (e.g. "agent not configured") without acting on it.
      if (msg.error) reject(new WSRequestError(msg.error, false));
      else resolve(msg as WSMessage<T>);
    });

    // A dropped connection dooms the in-flight request: fail fast instead of
    // burning the timeout. Ambiguous: the frame may have reached the server
    // before the socket died.
    handles.unsubDisc = ws.on("_disconnected", () => {
      cleanup(handles);
      reject(new WSRequestError(`Connection lost while waiting for the ${type} reply`, true));
    });

    // send() reports false when the socket is already down: the same doom as a
    // drop, known before the frame even left. Definite: the frame never left
    // the browser.
    if (!ws.send(type, payload, msgID)) {
      cleanup(handles);
      reject(new WSRequestError(`Not connected to the server — ${type} request was not sent`, false));
    }
  });
}
