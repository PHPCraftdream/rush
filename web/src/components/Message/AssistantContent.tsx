// Assistant-message content area: block grouping, tool activity groups and
// empty-response handling.

import { AntiHookComponent } from "@react-carburetor/core";
import type { Message as Msg, ContentPart, FinishPart } from "../../types";
import { busyCarburetor } from "../../state/busy";
import { messagesCarburetor } from "../../state/messages";
import { AssistantBlock } from "./AssistantBlock";
import { EditForm } from "./EditForm";
import { FinishErrorBlock } from "./FinishErrorBlock";
import { groupPartsIntoBlocks } from "./blocks";
import { extractText } from "./extractText";

/** Props of the assistant-message content area. */
interface IAssistantContentProps {
  message: Msg;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}

/**
 * Whether a part is something the operator can see (text, tool call/result, thinking).
 *
 * @param p - the message part
 */
const isVisiblePart = (p: ContentPart): boolean =>
  p.type === "text" || p.type === "tool_call" || p.type === "tool_result" || p.type === "thinking";

/**
 * Whether a part is a finish part.
 *
 * @param p - the message part
 */
const isFinishPart = (p: ContentPart): p is FinishPart => p.type === "finish";

/** Assistant-message content: the edit form, a placeholder, or the grouped blocks. */
export class AssistantContent extends AntiHookComponent<IAssistantContentProps> {
  /** Renders the edit form, an empty-response notice, or the message blocks. */
  public render() {
    const { message, editing, onSaveEdit, onCancelEdit } = this.props;
    const { messageBlockBreaks } = this.useCarburetor(messagesCarburetor);
    const { busySessions } = this.useCarburetor(busyCarburetor);
    const isLive = busySessions.includes(message.SessionID);

    // Fork patch: detect assistant messages that produced no visible content
    // (no text / tool_call / tool_result / thinking). This used to render as a
    // blank block in the WUI. We now show an explicit "empty response" notice
    // for finished turns and a "streaming…" placeholder for live turns.
    const hasVisibleContent = message.Parts.some(isVisiblePart);

    if (editing) {
      return (
        <EditForm
          initialValue={extractText(message.Parts)}
          rows={4}
          className="field-textarea text-[16px]"
          onSave={onSaveEdit}
          onCancel={onCancelEdit}
        />
      );
    }
    if (!hasVisibleContent) {
      // A turn that's still in flight may already carry a finish-part in the DB
      // (created speculatively by recovery / cancel paths and rewritten the
      // moment the first real delta arrives). Suppressing the "Empty response"
      // notice while busy avoids the flash where the placeholder shows for a
      // few hundred milliseconds and then disappears under the actual answer.
      const finishPart = message.Parts.find(isFinishPart);
      if (finishPart && !isLive) {
        const reason = finishPart.Reason ?? "unknown";
        const msg = finishPart.Message || "Empty response";
        const details = finishPart.Details
          || "The provider closed the stream without returning any content. Please retry.";
        return (
          <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
            <FinishErrorBlock reason={reason} message={msg} details={details} />
          </div>
        );
      }
      return (
        <div className="text-text-subtle leading-relaxed italic" style={{ fontSize: "var(--chat-font-size)" }}>
          {isLive ? "streaming…" : "(no content)"}
        </div>
      );
    }
    const blocks = groupPartsIntoBlocks(message.Parts, new Set(messageBlockBreaks[message.ID] ?? []));
    const rendered = [];
    for (let bi = 0; bi < blocks.length; bi++) {
      rendered.push(
        <AssistantBlock
          key={bi}
          block={blocks[bi]}
          index={bi}
          isLast={bi === blocks.length - 1}
          isLive={isLive}
          // partialWorkDone — used by the finish-part router to pick StreamPausedBlock
          // (soft amber) over FinishErrorBlock (red) when the watchdog stall happened
          // after the model already produced something substantive.
          partialWorkDone={hasVisibleContent}
          message={message}
        />,
      );
    }
    return (
      <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
        {rendered}
      </div>
    );
  }
}
