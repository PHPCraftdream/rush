/**
 * Why a correlated request failed, and whether it may nevertheless have happened.
 *
 * Distinguishes a DEFINITE failure (the request is known to have never
 * reached/executed on the server: an explicit error reply, or a send that never
 * left the browser) from an AMBIGUOUS one (a timeout, or a disconnect after
 * the frame was written) where the server may have already acted on it and the
 * client never heard back (task #1057). Callers that must not assume "nothing
 * happened", e.g. a queued message that would otherwise be resent, branch on
 * `ambiguous`.
 */
export class WSRequestError extends Error {
  public ambiguous: boolean;

  /**
   * Creates the error.
   *
   * @param message - failure text
   * @param ambiguous - whether the server may have acted on the request
   */
  public constructor(message: string, ambiguous: boolean) {
    super(message);
    this.name = "WSRequestError";
    this.ambiguous = ambiguous;
  }
}
