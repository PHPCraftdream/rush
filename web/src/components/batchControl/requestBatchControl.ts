import { wsRequest } from "../../ws";
import { batchControlCommands } from "./batchControlCommands";
import { batchControlPayload } from "./batchControlPayload";
import type { BatchCommandReply, BatchControlTarget } from "./batchControlTypes";

/**
 * Sends one batch control request and resolves with its reply frame.
 *
 * @param action - the control verb
 * @param target - the addressed session, root and node
 * @param items - items to add, for the add verb
 */
export async function requestBatchControl(
  action: keyof typeof batchControlCommands,
  target: BatchControlTarget,
  items?: unknown[],
): Promise<BatchCommandReply> {
  const payload = { ...batchControlPayload(target), ...(items ? { items } : {}) };
  return wsRequest(batchControlCommands[action], payload) as Promise<BatchCommandReply>;
}
