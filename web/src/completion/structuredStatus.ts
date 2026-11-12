import type { AsyncJobStatus } from "../asyncJob/asyncJobTypes";

/**
 * Maps a batch node state onto the AsyncJobStatus vocabulary the badges render.
 *
 * The states are batchTerminal's set in internal/server/handlers_batch.go. An
 * unknown state falls back to the outcome code ("ok" = finished), which is what
 * node-level completions carry.
 *
 * @param state - batch node state
 * @param outcomeCode - outcome code of the node
 */
export function structuredStatus(state: string, outcomeCode: string): AsyncJobStatus {
  switch (state) {
    case "completed":
      return "finished";
    case "failed":
      return "failed";
    case "timed_out":
      return "timed_out";
    case "cancelled":
    case "interrupted":
    case "skipped":
      return "cancelled";
    default:
      return outcomeCode === "ok" ? "finished" : "failed";
  }
}
