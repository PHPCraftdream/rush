// Projects the messages state down to what one SubAgentBlock needs.

import type { TReadonly } from "@react-carburetor/core";
import type { IMessagesState } from "../../state/MessagesCarburetor";
import type { Message, FinishPart } from "../../types";

/** The parent message (the one whose agent tool_call rendered the block), reduced to its finish state. */
export interface ISubAgentParent {
  /** The owning session, used to register the lazy load. */
  sessionID: string;
  /** The first non-partial finish part: its presence is the hard-kill "done" fallback. */
  terminalFinish: FinishPart | null;
  /** Whether any non-partial finish part ended in error or cancellation. */
  hasTerminalError: boolean;
}

/** The parent transcript's tool_result for the delegation, reduced to its metadata. */
export interface ISubAgentToolResult {
  metadata: string;
}

/** Plain data one SubAgentBlock renders from. */
export interface ISubAgentView {
  subMessages: Message[];
  parent: ISubAgentParent | null;
  toolResult: ISubAgentToolResult | null;
}

/**
 * Whether a finish part ended in error or cancellation.
 *
 * @param f - a finish part
 */
const endedBadly = (f: Readonly<FinishPart>): boolean => f.Reason === "error" || f.Reason === "canceled";

/**
 * Reduces the parent message to its finish state.
 *
 * @param parent - the parent message
 */
function reduceParent(parent: TReadonly<Message>): ISubAgentParent {
  let terminalFinish: FinishPart | null = null;
  let hasTerminalError = false;
  for (const p of parent.Parts) {
    if (p.type !== "finish" || p.Partial) continue;
    if (!terminalFinish) terminalFinish = p;
    if (endedBadly(p)) hasTerminalError = true;
  }
  return { sessionID: parent.SessionID, terminalFinish, hasTerminalError };
}

/**
 * Finds the parent transcript's tool_result for a tool call: it arrives on its
 * own role=tool message in the PARENT transcript (agent_turn.go's OnToolResult).
 *
 * @param messages - the active transcript
 * @param toolCallID - the delegation's tool call
 */
function findToolResult(
  messages: TReadonly<IMessagesState>["messages"],
  toolCallID: string,
): ISubAgentToolResult | null {
  for (const m of messages) {
    for (const p of m.Parts ?? []) {
      if (p.type === "tool_result" && p.ToolCallID === toolCallID) return { metadata: p.Metadata ?? "" };
    }
  }
  return null;
}

/**
 * Picks the sub-agent transcript, the parent's finish state and the
 * delegation's tool_result out of the messages state.
 *
 * @param data - the messages state
 * @param subSessionID - the sub-agent session key
 * @param messageID - the parent message id
 * @param toolCallID - the delegation's tool call
 */
export function selectSubAgentView(
  data: TReadonly<IMessagesState>,
  subSessionID: string,
  messageID: string,
  toolCallID: string,
): TReadonly<ISubAgentView> {
  let parent: ISubAgentParent | null = null;
  for (const m of data.messages) {
    if (m.ID === messageID) { parent = reduceParent(m); break; }
  }
  return {
    subMessages: data.subAgentMessages[subSessionID] ?? [],
    parent,
    toolResult: findToolResult(data.messages, toolCallID),
  };
}
