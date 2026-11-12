// Pairs a burst of tool/thinking parts into accordion rows.

import type { ContentPart } from "../../types";
import type { ActionItem } from "./ActionRow";

/** One burst item: a part plus where it came from. */
export interface IGroupSourceItem {
  part: ContentPart;
  idx: number;
  createdAt?: number;
  messageID?: string;
  partIndex: number;
  model?: string;
  effort?: string;
}

/** A sub-agent part held back from the accordion rows. */
export interface IRawAgentPart {
  part: ContentPart;
  idx: number;
  messageID?: string;
}

/** The accordion rows plus the sub-agent parts rendered after them. */
export interface IGroupedActions {
  actions: ActionItem[];
  rawAgentParts: IRawAgentPart[];
}

/**
 * Dedups consecutive tool actions with identical Name+Input (e.g. repeated
 * job_output polling). Keeps the LAST occurrence (freshest result) and
 * annotates it with repeatCount so the UI can show "×N".
 *
 * @param actions - the paired rows in order
 */
function dedupeRepeats(actions: readonly ActionItem[]): ActionItem[] {
  const deduped: ActionItem[] = [];
  for (const a of actions) {
    const prev = deduped[deduped.length - 1];
    if (
      prev &&
      a.kind === "tool" &&
      prev.kind === "tool" &&
      a.callPart &&
      prev.callPart &&
      a.callPart.Name === prev.callPart.Name &&
      a.callPart.Input === prev.callPart.Input
    ) {
      // Replace prev with current (fresher result), bump count
      a.repeatCount = (prev.repeatCount ?? 1) + 1;
      a.createdAt = prev.createdAt;
      deduped[deduped.length - 1] = a;
    } else {
      deduped.push(a);
    }
  }
  return deduped;
}

/**
 * Pairs calls and results by ToolCallID, in the order calls appear.
 *
 * An "agent" tool_call is passed through unchanged — SubAgentBlock owns its own
 * rendering and lives outside the row schema. Same for the matching tool_result
 * if any.
 *
 * @param items - the burst's parts in order
 */
export function groupToolActions(items: readonly IGroupSourceItem[]): IGroupedActions {
  const actionsOut: ActionItem[] = [];
  const rawAgentPartsOut: IRawAgentPart[] = [];
  const indexByCallID = new Map<string, number>();
  for (const { part, idx, createdAt, messageID, partIndex, model: partModel, effort: partEffort } of items) {
    if (part.type === "thinking") {
      const text = part.Thinking ?? "";
      // Per-row model/effort from the part's own source message (burst path);
      // falls back to group-level props (single-message path via AssistantContent).
      actionsOut.push({
        kind: "thinking", text, idx, key: `think-${idx}`, createdAt, messageID, partIndex,
        model: partModel, effort: partEffort,
      });
    } else if (part.type === "tool_call") {
      if (part.Name === "agent") { rawAgentPartsOut.push({ part, idx, messageID }); continue; }
      const a: ActionItem = { kind: "tool", callPart: part, idx, key: `call-${part.ID}`, createdAt };
      indexByCallID.set(part.ID, actionsOut.length);
      actionsOut.push(a);
    } else if (part.type === "tool_result") {
      if (part.Name === "agent") { rawAgentPartsOut.push({ part, idx, messageID }); continue; }
      const pos = indexByCallID.get(part.ToolCallID);
      if (pos !== undefined) {
        const slot = actionsOut[pos];
        if (slot.kind === "tool") slot.resultPart = part;
        // intentionally keep the earlier createdAt: that's when the action began
      } else {
        actionsOut.push({ kind: "tool", resultPart: part, idx, key: `res-${part.ToolCallID}-${idx}`, createdAt });
      }
    }
  }
  return { actions: dedupeRepeats(actionsOut), rawAgentParts: rawAgentPartsOut };
}
