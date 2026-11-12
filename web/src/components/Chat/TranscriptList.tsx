import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { Message as Msg } from "../../types";
import { IntermediateAssistantMessage, Message } from "../Message";
import type { RenderItem } from "./buildRenderItems";
import { ToolRun } from "./ToolRun";

/** Props of the transcript rows. */
interface ITranscriptListProps {
  renderItems: RenderItem[];
  sessionID: string;
  isBusy: boolean;
  selectedIDs: ReadonlySet<string>;
  forkDefaultTitle: string;
  onDeleteRequest: (id: string) => void;
  onRerunRequest: (id: string) => void;
  onRangeSelect: (index: number) => void;
  onSelectAbove: (index: number) => void;
  onSelectBelow: (index: number) => void;
}

/** The grouped transcript: messages, standalone narration and cross-message tool runs. */
export class TranscriptList extends AntiHookComponent<ITranscriptListProps> {
  /**
   * Renders one transcript row.
   *
   * @param item - the row
   * @param ri - its position among the rows
   * @param all - every row, to tell the last one
   */
  @bind
  protected renderItem(item: RenderItem, ri: number, all: readonly RenderItem[]) {
    const { sessionID, isBusy, selectedIDs, forkDefaultTitle } = this.props;
    if (item.kind === "standalonetext") {
      return (
        <IntermediateAssistantMessage
          key={`txt-${item.messageID}-${item.partIndex}-${ri}`}
          messageID={item.messageID}
          partIndex={item.partIndex}
          text={item.text}
          sessionID={sessionID}
        />
      );
    }
    if (item.kind === "toolrun") {
      // A toolrun is "current" only when nothing rendered AFTER it.
      // The moment the user sends a new message, that message
      // becomes the renderitem after this toolrun → isCurrent goes
      // false → the group's auto-rule collapses everything inside
      // (user-pinned rows survive via their own override).
      return (
        <ToolRun
          key={`run-${item.firstMsgID}-${ri}`}
          parts={item.parts}
          firstMsgID={item.firstMsgID}
          isLive={isBusy}
          isCurrent={ri === all.length - 1}
        />
      );
    }
    const m = item.message;
    return (
      <Message
        key={m.ID}
        index={item.index}
        // The cast hands the detached read-only message to a row that only reads it.
        message={m as Msg}
        onDeleteRequest={this.props.onDeleteRequest}
        onRerunRequest={this.props.onRerunRequest}
        onRangeSelect={this.props.onRangeSelect}
        onSelectAbove={this.props.onSelectAbove}
        onSelectBelow={this.props.onSelectBelow}
        selectionActive={selectedIDs.size > 0}
        isSelected={selectedIDs.has(m.ID)}
        forkDefaultTitle={forkDefaultTitle}
        sessionID={sessionID}
      />
    );
  }

  /** Renders every row in order. */
  public render() {
    return <>{this.props.renderItems.map(this.renderItem)}</>;
  }
}
