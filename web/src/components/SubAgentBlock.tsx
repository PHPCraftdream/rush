import { AntiHookComponent } from "@react-carburetor/core";
import { asyncJobStatuses } from "../state/asyncJobStatuses";
import { messagesCarburetor } from "../state/messages";
import { SubAgentFrame } from "./Message/SubAgentFrame";
import { findErrorFinish } from "./Message/findErrorFinish";
import { selectSubAgentView } from "./Message/selectSubAgentView";
import { summarizeSubTranscript } from "./Message/summarizeSubTranscript";

/** Props of the sub-agent block. */
interface ISubAgentBlockProps {
  messageID: string;
  toolCallID: string;
  prompt: string;
}

/**
 * Truncates the delegation prompt to the header label.
 *
 * @param prompt - the delegation prompt
 */
function promptLabel(prompt: string): string {
  if (!prompt) return "";
  const maxLen = 80;
  return prompt.length > maxLen ? prompt.slice(0, maxLen) + "..." : prompt;
}

/**
 * Whether the delegation's tool_result metadata says the run was async.
 *
 * @param metadata - the tool_result metadata JSON, empty when absent
 */
function startedAsync(metadata: string): boolean {
  if (!metadata) return false;
  try {
    return (JSON.parse(metadata) as { async?: boolean }).async === true;
  } catch {
    return false; // legacy metadata
  }
}

/** Reads one delegation's state out of the stores and renders it through SubAgentFrame. */
export class SubAgentBlock extends AntiHookComponent<ISubAgentBlockProps> {
  // The parent message (the one whose agent tool_call part rendered this
  // block) lives in the active transcript whenever this block is mounted.
  // Look it up by ID: its SessionID is the owning session used to register the
  // lazy load, and its finish state is the hard-kill fallback for `done`.
  private readonly view = this.connectSelection(
    () => messagesCarburetor,
    (data) => selectSubAgentView(data, this.subSessionID(), this.props.messageID, this.props.toolCallID),
  );

  /**
   * The sub-agent session key: `${messageID}$${toolCallID}`.
   *
   * Backend keys the sub-agent session that way (see
   * internal/session/session.go CreateAgentToolSessionID), and the WS layer
   * stores its messages under that exact composite ID (useWS.ts:
   * registerSubAgentSession / upsertSubAgentMessage). We MUST look up by the
   * same key — using toolCallID alone silently misses every event, which left
   * the block permanently empty ("Starting agent..."). Fall back to the bare
   * toolCallID only when messageID is unavailable (defensive; should not
   * happen for a live sub-agent).
   */
  private subSessionID(): string {
    const { messageID, toolCallID } = this.props;
    return messageID ? `${messageID}$$${toolCallID}` : toolCallID;
  }

  /**
   * Renders the delegation frame.
   *
   * "done" means this sub-agent's RUN is over — the `agent` tool returned.
   *
   * The parent tool_call part's Finished flag must NOT be used here: it means
   * "the model finished typing the arguments", not "the tool returned". It is
   * set twice while the provider stream is still being consumed, BEFORE any
   * tool is dispatched (internal/agent/agent_turn.go): FinishToolCall inside
   * OnToolInputEnd (~:1227), and Finished:true inline in OnToolCall's
   * AddToolCall (~:1261) — fantasy fires every OnToolCall for a step before
   * executing any tool. Both writes hit the DB before dispatch, so on the wire
   * the flag is already true before the sub-agent session even exists
   * (harness-verified: flag true at 0ms; the agent tool's Run() spans
   * 0-1200ms). Basing `done` on it collapsed the block ~1s into every
   * delegation and showed a green "done" badge for the whole run.
   *
   * - tool_result present (primary): the agent tool demonstrably returned. The
   *   result arrives on its own role=tool message in the PARENT transcript
   *   (agent_turn.go's OnToolResult creates it).
   * - parent terminally finished (fallback): a hard-killed run never gets its
   *   tool_result, but startup recovery stamps a terminal error finish on the
   *   parent's message (internal/app/app_recovery.go, "Process restarted").
   *   Partial checkpoint finishes on a live parent don't count, so this stays
   *   false mid-run.
   *
   * isRunning is !done. A busySessions lookup cannot be used here: a sub-agent
   * session ID NEVER enters $busySessions — the only feeder (the agent_busy
   * event) names client-chosen top-level sessions or the list_sessions
   * correction loop, whose SQL (internal/db/sql/sessions.sql, ListSessions)
   * filters `parent_session_id is NULL`, so sub-sessions are invisible to it.
   */
  public render() {
    const { toolCallID, prompt } = this.props;
    const { subMessages, parent, toolResult } = this.view();
    const asyncStatus = this.useComputed(asyncJobStatuses).get(toolCallID);
    const { completedCallIDs, model, effort } = summarizeSubTranscript(subMessages);

    const toolResultArrived = !!toolResult;
    const asyncStarted = toolResult ? startedAsync(toolResult.metadata) : false;
    const parentDone = !!parent?.terminalFinish;
    const parentError = parent?.hasTerminalError ?? false;
    const done = asyncStarted ? !!asyncStatus || parentError : toolResultArrived || parentDone;

    return (
      <SubAgentFrame
        toolCallID={toolCallID}
        subSessionID={this.subSessionID()}
        ownerSessionID={parent?.sessionID}
        messages={subMessages}
        completedCallIDs={completedCallIDs}
        model={model}
        effort={effort}
        label={promptLabel(prompt)}
        done={done}
        asyncStatus={asyncStatus}
        errorFinish={findErrorFinish(subMessages, toolResultArrived, parent)}
      />
    );
  }
}
