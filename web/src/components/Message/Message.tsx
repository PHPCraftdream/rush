// The top-level chat message row: role switch, selection, hover actions.

import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Check, Star } from "lucide-react";
import type { Message as Msg, ContentPart } from "../../types";
import { updateMessageContent } from "../../store";
import { busyCarburetor } from "../../state/busy";
import { selectionCarburetor } from "../../state/selection";
import { ForkSessionModal } from "../ForkSessionModal";
import { AssistantContent } from "./AssistantContent";
import { AssistantHoverActions } from "./AssistantHoverActions";
import { BackgroundJobNotice } from "./BackgroundJobNotice";
import { CheckboxContextMenu } from "./CheckboxContextMenu";
import { SummaryMessage } from "./SummaryMessage";
import { TimeBadge } from "./TimeBadge";
import { UserContent } from "./UserContent";
import { UserHoverActions } from "./UserHoverActions";
import { extractText } from "./extractText";
import { isTerminallyFinished } from "./isTerminallyFinished";
import { isAsyncCompletionNotice } from "../../asyncJobCompletion";

// ── Message ───────────────────────────────────────────────────────────────────

/** Props of a message row. */
export interface MessageProps {
  message: Msg;
  onDeleteRequest: (id: string) => void;
  onRerunRequest: (id: string) => void;
  onRangeSelect: (index: number) => void;
  onSelectAbove: (index: number) => void;
  onSelectBelow: (index: number) => void;
  selectionActive: boolean;
  isSelected: boolean;
  forkDefaultTitle: string;
  sessionID: string;
  index: number;
}

/** Local state of a message row. */
interface IMessageState {
  editing: boolean;
  forking: boolean;
  hovered: boolean;
  contextMenu: { x: number; y: number } | null;
}

/**
 * Whether a part counts as assistant content for the hover strip.
 *
 * @param p - the message part
 */
const isActionablePart = (p: ContentPart): boolean =>
  p.type === "text" || p.type === "tool_call" || p.type === "tool_result" || p.type === "finish";

/** A chat message row: role-specific content, selection checkbox and hover actions. */
export class Message extends AntiHookComponent<MessageProps, IMessageState> {
  public state: IMessageState = { editing: false, forking: false, hovered: false, contextMenu: null };

  /**
   * Whether the message is safe to edit/delete/select: user, or terminally finished.
   *
   * An assistant message with no terminal Finish part is still owned by an
   * in-flight agent turn. User messages are never streamed, so they are
   * always editable/deletable/selectable — this single flag gates all three
   * controls for an assistant message.
   *
   *   - Edit: the server refuses edits to a still-streaming message (task
   *     #590, handlers_messages.go's updateMessageAndVerify) because the
   *     turn's next checkpoint or terminal write would silently overwrite it.
   *   - Delete / selection-for-bulk-delete (task #595): the server now also
   *     refuses to delete a still-streaming assistant message
   *     (message.Service.Delete / DeleteMessageIfTerminal) because the live
   *     turn keeps writing to the same row after a delete would otherwise
   *     "resurrect" it in the UI (see docs/reviews/2026-08-19-release-
   *     readiness-static-follow-up-d3ee9841.md P1-1). Hiding Trash and the
   *     selection checkbox here means an operator never hits that refusal in
   *     the first place, and a still-streaming message can never end up
   *     inside a bulk delete_messages payload.
   *
   *   - Orphan exception: an unfinished assistant message in an IDLE session
   *     (i.e., the turn crashed or was killed) is considered an orphan and
   *     can be deleted/selected — the server's delete handler proves the
   *     session is idle via IsSessionBusy before force-deleting it. A BUSY
   *     session's live stream is still guarded.
   *
   * @param isUser - whether the message is a user message
   */
  private streamGuardOK(isUser: boolean): boolean {
    return isUser || isTerminallyFinished(this.props.message.Parts);
  }

  /**
   * Whether the message may be selected: stream-guarded, or an orphan of a
   * crashed/killed turn. Read untracked for event handlers; render subscribes
   * to the busy set itself.
   */
  private isSelectableNow(): boolean {
    const { message } = this.props;
    const isUser = message.Role === "user";
    const isOrphan = !isUser && !isTerminallyFinished(message.Parts) && !busyCarburetor.isBusy(message.SessionID);
    return this.streamGuardOK(isUser) || isOrphan;
  }

  /** Marks the row hovered. */
  @bind
  protected handleMouseEnter(): void {
    this.setState({ hovered: true });
  }

  /** Marks the row not hovered. */
  @bind
  protected handleMouseLeave(): void {
    this.setState({ hovered: false });
  }

  /** Opens the fork dialog. */
  @bind
  protected handleForkOpen(): void {
    this.setState({ forking: true });
  }

  /** Closes the fork dialog. */
  @bind
  protected handleForkClose(): void {
    this.setState({ forking: false });
  }

  /** Enters edit mode. */
  @bind
  protected handleEditOpen(): void {
    this.setState({ editing: true });
  }

  /** Leaves edit mode without saving. */
  @bind
  protected handleEditClose(): void {
    this.setState({ editing: false });
  }

  /** Asks the list to delete this message. */
  @bind
  protected handleDelete(): void {
    this.props.onDeleteRequest(this.props.message.ID);
  }

  /** Asks the list to rerun this message. */
  @bind
  protected handleRerun(): void {
    this.props.onRerunRequest(this.props.message.ID);
  }

  /**
   * Persists a changed message text and leaves edit mode.
   *
   * @param text - the edited text
   */
  @bind
  protected handleSaveEdit(text: string): void {
    const { message } = this.props;
    if (text && text !== extractText(message.Parts)) updateMessageContent(message.ID, text);
    this.setState({ editing: false });
  }

  /**
   * Toggles the selection, or range-selects with Shift.
   *
   * task #595: a still-streaming assistant message can never be selected, so
   * it can never end up inside a delete_messages bulk payload — see
   * streamGuardOK above. This mirrors "hide/disable Trash and selection-delete
   * until the message is terminally finished" from the P1-1 fix: selection is
   * the OTHER path that reaches bulk delete besides the single Trash button.
   *
   * Orphan exception: an unfinished assistant message in an IDLE session (i.e.,
   * the turn crashed or was killed) can be selected for deletion — the server
   * proves the session is idle before force-deleting.
   *
   * @param e - the click event
   */
  @bind
  protected handleCheckboxClick(e: MouseEvent): void {
    const { message, index, onRangeSelect } = this.props;
    if (!this.isSelectableNow()) return;
    if (e.shiftKey) {
      e.preventDefault(); // Prevent text selection between clicks
      onRangeSelect(index);
    }
    else { selectionCarburetor.toggle(message.ID); }
  }

  /**
   * Opens the select-above/below menu at the pointer.
   *
   * @param e - the contextmenu event
   */
  @bind
  protected handleCheckboxContextMenu(e: MouseEvent): void {
    if (!this.isSelectableNow()) return;
    e.preventDefault();
    this.setState({ contextMenu: { x: e.clientX, y: e.clientY } });
  }

  /** Closes the select-above/below menu. */
  @bind
  protected closeContextMenu(): void {
    this.setState({ contextMenu: null });
  }

  /** Selects everything above this message and closes the menu. */
  @bind
  protected handleSelectAbove(): void {
    this.props.onSelectAbove(this.props.index);
    this.setState({ contextMenu: null });
  }

  /** Selects everything below this message and closes the menu. */
  @bind
  protected handleSelectBelow(): void {
    this.props.onSelectBelow(this.props.index);
    this.setState({ contextMenu: null });
  }

  /** Renders the row: checkbox, role-specific content, action strip and dialogs. */
  public render() {
    const {
      message, selectionActive, isSelected, forkDefaultTitle, sessionID,
    } = this.props;
    const { editing, forking, hovered, contextMenu } = this.state;
    const { busySessions } = this.useCarburetor(busyCarburetor);

    if (message.Hidden) return null;
    if (message.IsSummaryMessage) return <SummaryMessage message={message} />;
    if (message.BackgroundJobNotice || isAsyncCompletionNotice(message)) {
      return <BackgroundJobNotice message={message} />;
    }

    const isUser = message.Role === "user";

    const copyText = extractText(message.Parts);
    const hasContent = !isUser && message.Parts.some(isActionablePart);

    // Orphan: an assistant message that is NOT terminally finished AND its
    // session is NOT busy. This represents a message from a crashed/killed turn
    // — the server's delete handler force-deletes it after proving the session
    // is idle via IsSessionBusy.
    const isOrphan = !isUser && !isTerminallyFinished(message.Parts) && !busySessions.includes(message.SessionID);

    const streamGuardOK = this.streamGuardOK(isUser);
    const editable = streamGuardOK;
    const deletable = streamGuardOK || isOrphan;
    const selectable = streamGuardOK || isOrphan;

    // Checkbox is always in DOM (reserves layout space), opacity-0 when not
    // relevant. A non-selectable (still-streaming) message never shows it,
    // regardless of hover/selection-mode state — there is nothing useful the
    // control could do (see handleCheckboxClick's early return above).
    // Orphan exception: an unfinished assistant message in an IDLE session
    // (i.e., the turn crashed or was killed) can be selected for deletion.
    const checkboxVisible = selectable && (selectionActive || isSelected || hovered);
    const rowTone = isSelected ? "bg-accent/5" : "";
    const pinTone = message.Pinned ? "border-l-4 border-yellow/60 bg-yellow/[5%]" : "";
    const boxTone = isSelected ? "bg-accent border-accent" : "border-text-subtle/50 hover:border-accent";

    return (
      <div
        id={`msg-${message.ID}`}
        data-msg-role={isUser ? "user" : "assistant"}
        className={`msg-row flex flex-col px-5 py-3 transition-colors ${rowTone} ${pinTone}`}
        onMouseEnter={this.handleMouseEnter}
        onMouseLeave={this.handleMouseLeave}
      >
        <div className="flex gap-3">
          <div
            className={`msg-checkbox-wrap ${checkboxVisible ? "opacity-100" : "opacity-0 pointer-events-none"}`}
            onClick={this.handleCheckboxClick}
            onContextMenu={this.handleCheckboxContextMenu}
          >
            <div className={`msg-checkbox ${boxTone}`}>
              {isSelected && <Check size={10} className="text-white shrink-0" />}
            </div>
          </div>
          <div className={`flex flex-1 min-w-0 ${isUser ? "justify-end" : "justify-start"}`}>
            {isUser ? (
              <div className="max-w-[80%]">
                <UserContent
                  message={message}
                  editing={editing}
                  onSaveEdit={this.handleSaveEdit}
                  onCancelEdit={this.handleEditClose}
                />
              </div>
            ) : (
              <div className="w-full min-w-0">
                <AssistantContent
                  message={message}
                  editing={editing}
                  onSaveEdit={this.handleSaveEdit}
                  onCancelEdit={this.handleEditClose}
                />
              </div>
            )}
          </div>
        </div>

        {/* Action strip — fixed-height row; interactive controls only mounted on hover */}
        {!editing && (
          <div className={`msg-actions ${isUser ? "justify-end" : "justify-start"}`}>
            {/* Star is always visible for pinned messages, regardless of hover state */}
            {message.Pinned && (
              <Star
                size={13}
                className={`text-yellow shrink-0 ${isUser ? "order-last ml-2" : "order-first mr-2"}`}
                fill="currentColor"
              />
            )}
            {/* Timestamp is always visible (not gated on hover), unlike the
                copy/edit/delete/fork/star controls below. */}
            <TimeBadge epochSec={message.CreatedAt} />
            {hovered && (
              isUser ? (
                <UserHoverActions
                  messageID={message.ID}
                  copyText={copyText}
                  isPinned={message.Pinned}
                  onEdit={this.handleEditOpen}
                  onDelete={this.handleDelete}
                  onFork={this.handleForkOpen}
                  onRerun={this.handleRerun}
                />
              ) : hasContent ? (
                <AssistantHoverActions
                  message={message}
                  copyText={copyText}
                  editable={editable}
                  deletable={deletable}
                  onEdit={this.handleEditOpen}
                  onDelete={this.handleDelete}
                  onFork={this.handleForkOpen}
                />
              ) : null
            )}
          </div>
        )}

        {forking && (
          <ForkSessionModal sessionID={sessionID} defaultTitle={forkDefaultTitle} onClose={this.handleForkClose} />
        )}

        {contextMenu && (
          <CheckboxContextMenu
            x={contextMenu.x}
            y={contextMenu.y}
            onSelectAbove={this.handleSelectAbove}
            onSelectBelow={this.handleSelectBelow}
            onClose={this.closeContextMenu}
          />
        )}
      </div>
    );
  }
}
