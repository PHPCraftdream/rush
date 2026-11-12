import { wsRequest } from "../net/wsRequest";
import { sendQueuedItem } from "../queueOnIdle/sendQueuedItem";
import { queueCarburetor } from "../state/queue";
import type { QueuedMessage } from "../state/QueueCarburetor";
import { describeSendFailure } from "./describeSendFailure";
import { queuedMessagePayload } from "./queuedMessagePayload";
import { sendFailureState } from "./sendFailureState";

/**
 * inject_message has no server-side bound (a DB write + in-memory mailbox
 * enqueue, no provider call) but does save attachments to disk first: more
 * headroom than the 10s default for a large attachment on a slow disk.
 */
const INJECT_MESSAGE_TIMEOUT_MS = 15_000;

/** A described send failure, ready to restore into the queue. */
interface ISendFailure {
  message: string;
  state: ReturnType<typeof sendFailureState>;
}

/**
 * Takes one queued item out of the queue.
 *
 * @param sessionID - session id
 * @param id - queued message id
 */
const takeQueued = (sessionID: string, id: string): { item: QueuedMessage; index: number } | undefined =>
  queueCarburetor.take(sessionID, id);

/**
 * Requests an inject_message for a queued item.
 *
 * @param sessionID - session id
 * @param item - the queued message
 */
const requestInject = (sessionID: string, item: QueuedMessage): Promise<unknown> =>
  wsRequest("inject_message", queuedMessagePayload(sessionID, item), { timeoutMs: INJECT_MESSAGE_TIMEOUT_MS });

/**
 * Restores a queued item after a failed send.
 *
 * @param sessionID - session id
 * @param item - the taken message
 * @param index - its original position
 * @param failure - the described failure
 */
const restoreFailed = (sessionID: string, item: QueuedMessage, index: number, failure: ISendFailure): void =>
  queueCarburetor.restoreItem(sessionID, item, index, failure.message, failure.state);

/**
 * Describes a rejection for the queue UI.
 *
 * @param err - what the request rejected with
 */
const describeFailure = (err: unknown): ISendFailure => ({
  message: describeSendFailure(err),
  state: sendFailureState(err),
});

/**
 * Sends one queued message right now via inject_message.
 *
 * It merges into the next step of the CURRENT turn without interrupting it.
 * Atomicity contract shared with interruptAndSendQueuedMessage: the message is
 * removed from the queue SYNCHRONOUSLY, before the wsRequest round-trip even
 * starts. Store updates and WS event handling both run on the main thread, so
 * by the time any later event (in particular the agent_busy=false handler in
 * connectWS, which drains the whole queue) runs, the item is already gone: it
 * can never be flushed a second time, no matter how the send and a busy-flip
 * interleave. A failed request restores the item to its original index.
 * Restored failed or ambiguous items must not be automatically retried.
 *
 * @param sessionID - session id
 * @param id - queued message id
 */
export async function sendQueuedMessageNow(sessionID: string, id: string): Promise<void> {
  return sendQueuedItem(sessionID, id, {
    take: takeQueued,
    request: requestInject,
    restore: restoreFailed,
    describeFailure,
  });
}
