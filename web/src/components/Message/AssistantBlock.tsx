// One zebra-stripe block of an assistant message: a tool activity group or a run of parts.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { Message as Msg, ContentPart } from "../../types";
import { Part } from "./Part";
import { ToolActivityGroup } from "./ToolActivityGroup";
import type { VisualBlock } from "./blocks";

/** Props of an assistant block. */
interface IAssistantBlockProps {
  block: VisualBlock;
  index: number;
  isLast: boolean;
  isLive: boolean;
  partialWorkDone: boolean;
  message: Msg;
}

/** One block of an assistant message. */
export class AssistantBlock extends AntiHookComponent<IAssistantBlockProps> {
  /**
   * Tags a tool part with its message and part index for the activity group.
   *
   * @param it - the block item
   * @param it.part - the message part
   * @param it.idx - the part's index within the message
   */
  @bind
  protected toGroupItem(it: { part: ContentPart; idx: number }) {
    return { ...it, messageID: this.props.message.ID, partIndex: it.idx };
  }

  /**
   * Renders one part of a non-tool block.
   *
   * @param item - the block item
   * @param item.part - the message part
   * @param item.idx - the part's index within the message
   */
  @bind
  protected renderItem({ part, idx }: { part: ContentPart; idx: number }) {
    const { block, message, partialWorkDone } = this.props;
    return (
      <Part
        key={idx}
        part={part}
        index={idx}
        isUser={false}
        messageID={message.ID}
        thinkingDone={block.thinkingDone}
        partialWorkDone={partialWorkDone}
        model={message.Model}
        effort={message.ReasoningEffort}
        sessionID={message.SessionID}
      />
    );
  }

  /** Renders the block wrapper and its content. */
  public render() {
    const { block, index, isLast, isLive, message } = this.props;
    return (
      <div className={index > 0 ? "msg-block-sep" : undefined}>
        {block.kind === "tool" ? (
          // Whole tool burst is rendered through the accordion group: one
          // collapsible row per call+result pair, last row open by default
          // (the "current action"), prior rows collapsed. User can pin any
          // row open/closed and the auto-rule stops touching that row.
          //
          // isCurrent marks the group the auto-rule should leave open. It
          // is the LAST block of a message that is still streaming: while
          // the turn runs, that burst is the work happening right now.
          // Once the turn ends, isLive drops and every group collapses —
          // the transcript settles into history. Earlier blocks of a live
          // message are already past, so they collapse too.
          //
          // The prop used to be omitted entirely, which the auto-rule read
          // as falsy and collapsed unconditionally, contradicting the
          // paragraph above and diverging from Chat.tsx, which has always
          // passed it.
          <ToolActivityGroup
            items={block.items.map(this.toGroupItem)}
            live={isLive}
            isCurrent={isLive && isLast}
            model={message.Model}
            effort={message.ReasoningEffort}
          />
        ) : (
          block.items.map(this.renderItem)
        )}
      </div>
    );
  }
}
