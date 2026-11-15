import type { AsyncJobStatus } from "./asyncJobTypes";

/**
 * Maps the cause wording of a stopped notice onto a status.
 *
 * @param cause - the cause phrase captured from the notice
 */
export function stoppedStatus(cause: string): AsyncJobStatus {
  if (cause.startsWith("timed out")) return "timed_out";
  if (cause.includes("batch_stop")) return "stopped";
  return "cancelled";
}
