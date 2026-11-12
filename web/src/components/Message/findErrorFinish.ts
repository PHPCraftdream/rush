// Finds the terminal bad finish of a sub-agent run.

import type { TReadonly } from "@react-carburetor/core";
import type { Message, FinishPart } from "../../types";
import type { ISubAgentParent } from "./selectSubAgentView";

/**
 * Whether a message is an assistant message.
 *
 * @param m - a message
 */
const isAssistant = (m: TReadonly<Message>): boolean => m.Role === "assistant";

/**
 * Whether a part is a non-partial finish part.
 *
 * @param p - a message part
 */
const isTerminalFinishPart = (p: TReadonly<Message>["Parts"][number]): p is TReadonly<FinishPart> =>
  p.type === "finish" && !p.Partial;

/**
 * Returns the finish when it is an error or cancellation.
 *
 * @param f - a finish part, if any
 */
const badFinish = (f: TReadonly<FinishPart> | undefined): TReadonly<FinishPart> | undefined =>
  f && (f.Reason === "error" || f.Reason === "canceled") ? f : undefined;

/**
 * Finds the error/canceled finish of a run, so it does not wear the green "done" badge.
 *
 * Reads the
 * terminal finish of the sub-agent's last assistant message and apply the same
 * rule as the main renderer's finish router (Message/Part.tsx): error or
 * canceled renders an error block. When the tool call never returned (no
 * tool_result for this call — a hard kill), attribute the parent's terminal
 * error finish instead; recovery stamps "Process restarted" there and the
 * sub-agent's own transcript may have no finish at all.
 *
 * @param messages - the sub-agent transcript
 * @param toolResultArrived - whether the delegation's tool_result is in the parent transcript
 * @param parent - the parent message's finish state, if found
 */
export function findErrorFinish(
  messages: readonly TReadonly<Message>[],
  toolResultArrived: boolean,
  parent: TReadonly<ISubAgentParent> | null,
): TReadonly<FinishPart> | undefined {
  const lastAssistant = messages.filter(isAssistant).pop();
  if (lastAssistant) {
    const f = badFinish((lastAssistant.Parts ?? []).find(isTerminalFinishPart));
    if (f) return f;
  }
  if (!toolResultArrived && parent) return badFinish(parent.terminalFinish ?? undefined);
  return undefined;
}
