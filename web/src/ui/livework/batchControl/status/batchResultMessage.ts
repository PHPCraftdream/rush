import type { BatchControlResult } from "../payload/batchControlTypes";

/**
 * Describes a control result for the operator.
 *
 * @param result - the server's verdict
 */
export function batchResultMessage(result: BatchControlResult): string {
  return result.ok ? result.result : `Refused: ${result.result}`;
}
