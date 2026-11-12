import { ws } from "../net/client";

/** A client-side error as the backend logs it. */
interface ErrorPayload {
  message: string;
  source?: string;
  stack?: string;
}

/** Errors that occurred before the WS was open; flushed on connect. */
const errorBuffer: ErrorPayload[] = [];
let wsReady = false;

/** Sends every buffered error. */
const flushErrors = (): void => {
  while (errorBuffer.length) {
    ws.send("log_client_error", errorBuffer.shift());
  }
};

/**
 * Sends an error now, or buffers it until the WS connects.
 *
 * @param payload - the error
 */
const sendError = (payload: ErrorPayload): void => {
  if (wsReady) {
    ws.send("log_client_error", payload);
  } else {
    errorBuffer.push(payload);
  }
};

/**
 * Forwards an uncaught JS error to the backend.
 *
 * @param ev - the window error event
 */
const onWindowError = (ev: ErrorEvent): void => {
  sendError({
    message: ev.message ?? String(ev.error),
    source: ev.filename ? `${ev.filename}:${ev.lineno}:${ev.colno}` : undefined,
    stack: ev.error?.stack,
  });
};

/**
 * Forwards an unhandled promise rejection to the backend.
 *
 * @param ev - the rejection event
 */
const onUnhandledRejection = (ev: PromiseRejectionEvent): void => {
  const reason = ev.reason;
  sendError({
    message: reason?.message ?? String(reason),
    stack: reason?.stack,
  });
};

/** Marks the WS ready and flushes errors buffered before it opened. */
const onConnected = (): void => {
  wsReady = true;
  flushErrors();
};

/** Marks the WS not ready. */
const onDisconnected = (): void => {
  wsReady = false;
};

/**
 * Forwards all uncaught JS errors and unhandled rejections to the backend; call once at startup.
 *
 * Errors raised before the WS is open are buffered and flushed on connect.
 */
export function setupTelemetry(): void {
  ws.on("_connected", onConnected);
  ws.on("_disconnected", onDisconnected);
  window.addEventListener("error", onWindowError);
  window.addEventListener("unhandledrejection", onUnhandledRejection);
}
