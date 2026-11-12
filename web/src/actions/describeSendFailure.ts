import { WSRequestError } from "../net/WSRequestError";

/**
 * Turns a wsRequest rejection into a short, UI-ready message.
 *
 * A DEFINITE failure (server explicitly rejected the request, or the frame
 * never left the browser) keeps its own text: the message is known not to have
 * gone out. An AMBIGUOUS one (timeout, or a disconnect after the frame was
 * written) means the server may have already acted on it, so resending could
 * duplicate it: the message says so instead of "failed".
 *
 * @param err - what the request rejected with
 */
export function describeSendFailure(err: unknown): string {
  if (err instanceof WSRequestError && err.ambiguous) {
    return "May already be sent — check chat before resending.";
  }
  return err instanceof Error ? err.message : String(err);
}
