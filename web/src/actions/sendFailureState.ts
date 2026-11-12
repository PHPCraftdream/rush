import { WSRequestError } from "../net/WSRequestError";

/**
 * Classifies a send failure: ambiguous errors may already have gone out, everything else was rejected.
 *
 * @param err - what the request rejected with
 */
export function sendFailureState(err: unknown): "rejected" | "ambiguous" {
  return err instanceof WSRequestError && err.ambiguous ? "ambiguous" : "rejected";
}
