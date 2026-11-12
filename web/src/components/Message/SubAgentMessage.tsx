// One assistant message of a sub-agent transcript: its tool calls and prose.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import ReactMarkdown from "react-markdown";
import type { Message, ContentPart } from "../../types";
import { formatActionArgs } from "../../toolFormat";
import { TimeBadge } from "./TimeBadge";
import { MD_REMARK } from "./mdRemarkPlugins";
import { MD_REHYPE } from "./mdRehypePlugins";
import { extractText } from "./extractText";

/** Props of a sub-agent message. */
interface ISubAgentMessageProps {
  message: TReadonly<Message>;
  completed: Set<string>;
}

/** A tool call part. */
type ToolCallPart = TReadonly<ContentPart & { type: "tool_call" }>;

/**
 * Whether a part is a tool call.
 *
 * @param p - a message part
 */
const isToolCall = (p: TReadonly<ContentPart>): p is ToolCallPart => p.type === "tool_call";

/** Tool-call rows and markdown prose of one sub-agent message; renders nothing when empty. */
export class SubAgentMessage extends AntiHookComponent<ISubAgentMessageProps> {
  /**
   * Renders one tool call row. No per-tool-call timestamp exists in the
   * sub-agent transcript; tool calls are grouped within one message, so the
   * owning message's CreatedAt is the best available approximation.
   *
   * @param tc - the tool call part
   * @param i - the row index
   */
  @bind
  protected renderToolCall(tc: ToolCallPart, i: number) {
    const { message, completed } = this.props;
    const subject = formatActionArgs(tc.Name, tc.Input);
    return (
      <div key={i} className="flex items-center gap-1.5 text-xs text-text-subtle py-0.5">
        <span className="text-mauve font-semibold">{tc.Name}</span>
        <span className="text-text font-mono truncate flex-1 min-w-0">{subject}</span>
        {!completed.has(tc.ID) && <span className="animate-pulse shrink-0">running...</span>}
        <TimeBadge epochSec={message.CreatedAt} />
      </div>
    );
  }

  /** Renders the tool calls and the prose. */
  public render() {
    const { message } = this.props;
    const parts = message.Parts ?? [];
    const text = extractText(parts);
    const toolCalls = parts.filter(isToolCall);

    if (!text && toolCalls.length === 0) return null;

    return (
      <div className="py-1">
        {toolCalls.map(this.renderToolCall)}
        {text && (
          <div className="md text-sm text-text-muted">
            <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
          </div>
        )}
      </div>
    );
  }
}
