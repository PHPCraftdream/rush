import { AntiHookComponent } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { ContentPart, Message as Msg } from "../../types";
import { messagesCarburetor } from "../../state/messages";
import { ToolActivityGroup } from "../Message";
import type { BurstPart } from "./buildRenderItems";

/** Props of one cross-message tool run. */
interface IToolRunProps {
  parts: BurstPart[];
  firstMsgID: string;
  isLive: boolean;
  isCurrent: boolean;
}

/**
 * Finds when the run's first message was created.
 *
 * @param messages - the transcript
 * @param id - id of the run's first message
 */
const findCreatedAt = (messages: TReadonly<Msg[]>, id: string): number | undefined => {
  for (let i = 0; i < messages.length; i++) {
    if (messages[i].ID === id) return messages[i].CreatedAt;
  }
  return undefined;
};

/**
 * Turns a burst part into a ToolActivityGroup item.
 *
 * `idx` is the burst position (for React keys/grouping ONLY); `partIndex` is the real index into the
 * source message's Parts array (the ONLY field valid for server addressing via WS commands like
 * update_message_part/delete_message_part). `createdAt` travels with each part so action rows can
 * render per-row timestamps. The cast hands the detached read-only part to the group, which only reads it.
 *
 * @param bp - the burst part
 * @param idx - its position in the burst
 */
const toGroupItem = (bp: BurstPart, idx: number) => ({
  part: bp.part as ContentPart,
  idx,
  createdAt: bp.createdAt,
  messageID: bp.messageID,
  partIndex: bp.partIndex,
  model: bp.model,
  effort: bp.effort,
});

/** One accordion of consecutive tool activity, possibly spanning several messages. */
export class ToolRun extends AntiHookComponent<IToolRunProps> {
  /** Renders the run; ToolActivityGroup pairs call and result by ToolCallID itself. */
  public render() {
    const { parts, firstMsgID, isLive, isCurrent } = this.props;
    const { messages } = this.useCarburetor(messagesCarburetor);
    const startedAt = firstMsgID ? findCreatedAt(messages, firstMsgID) : undefined;
    return (
      <div
        id={firstMsgID ? `msg-${firstMsgID}` : undefined}
        data-msg-role="assistant"
        data-tool-run="true"
        className="msg-row flex flex-col px-5 py-3"
        title={`${parts.length} tool parts grouped across messages`}
      >
        <div className="w-full min-w-0">
          <ToolActivityGroup items={parts.map(toGroupItem)} live={isLive} isCurrent={isCurrent} startedAt={startedAt} />
        </div>
      </div>
    );
  }
}
