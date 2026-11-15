import { ws } from "../../../backend/ws/client";

/**
 * Cancels a queued summarize request.
 *
 * @param sessionID - session id
 */
export function cancelQueuedSummarize(sessionID: string): void {
  ws.send("cancel_queued_summarize", { sessionID });
}
