import type { BatchWorkNode } from "../types";
import { wsRequest } from "../ws";

export interface BatchControlTarget { sessionID: string; rootID?: string; nodeID?: string; waitMS?: number }
export interface BatchControlResult { sessionID: string; rootID?: string; nodeID?: string; action: string; result: string; ok: boolean; outcomeSummary?: string; responseEnvelope?: string; outcomeRef?: string; requestID?: string; answerCode?: string }
export interface BatchCommandReply { type: string; payload?: unknown }
export const batchControlCommands = { status: "batch_status", output: "batch_output", stop: "batch_stop", add: "batch_add", drain: "batch_drain" } as const;

export function batchControlPayload(target: BatchControlTarget) {
  return { sessionID: target.sessionID, rootID: target.rootID ?? "", nodeID: target.nodeID ?? "", ...(target.waitMS === undefined ? {} : { waitMS: target.waitMS }) };
}
export function batchResultMessage(result: BatchControlResult): string {
  return result.ok ? result.result : `Refused: ${result.result}`;
}
export function batchNodeStatus(node: Pick<BatchWorkNode, "state">): string { return node.state; }
export function isStopConfirmed(node: Pick<BatchWorkNode, "state">): boolean { return ["cancelled", "completed", "failed", "timed_out", "interrupted", "skipped"].includes(node.state); }
export function batchNodeNotice(node: Pick<BatchWorkNode, "noticeSelected" | "noticeState" | "state">): string {
  if (node.noticeSelected && node.noticeState === "pending") return "Delayed notice";
  if (!node.noticeSelected && isStopConfirmed(node)) return "Notice suppressed";
  return "";
}
export async function requestBatchControl(action: keyof typeof batchControlCommands, target: BatchControlTarget, items?: unknown[]): Promise<BatchCommandReply> {
  return wsRequest(batchControlCommands[action], { ...batchControlPayload(target), ...(items ? { items } : {}) }) as Promise<BatchCommandReply>;
}
