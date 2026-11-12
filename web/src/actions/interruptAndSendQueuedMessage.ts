import { wsRequest } from "../net/wsRequest";
import { queueCarburetor } from "../state/queue";
import { describeSendFailure } from "./describeSendFailure";
import { queuedMessagePayload } from "./queuedMessagePayload";
import { sendFailureState } from "./sendFailureState";

/**
 * interrupt_and_send bounds its own server-side work at 30s (handleInterruptAndSend
 * context.WithTimeout, cancelling a turn that may be stuck inside a tool call).
 * This must sit ABOVE that bound, or a merely-slow-but-successful interrupt
 * times out client-side while the server still delivers it, and the item gets
 * restored with an error for a message that already went out (task #1057 review).
 */
const INTERRUPT_AND_SEND_TIMEOUT_MS = 40_000;

/**
 * Sends one queued message via interrupt_and_send: cancels the running turn and starts a new one with it.
 *
 * Same atomicity contract as sendQueuedMessageNow.
 *
 * @param sessionID - session id
 * @param id - queued message id
 */
export async function interruptAndSendQueuedMessage(sessionID: string, id: string): Promise<void> {
  const taken = queueCarburetor.take(sessionID, id);
  if (!taken) return;
  try {
    await wsRequest("interrupt_and_send", queuedMessagePayload(sessionID, taken.item), {
      timeoutMs: INTERRUPT_AND_SEND_TIMEOUT_MS,
    });
  } catch (err) {
    queueCarburetor.restoreItem(sessionID, taken.item, taken.index, describeSendFailure(err), sendFailureState(err));
  }
}
