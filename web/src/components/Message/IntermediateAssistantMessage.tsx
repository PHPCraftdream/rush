// Renders an intermediate text part extracted from a tool-bearing assistant message.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import { GitFork, Pencil, Trash2 } from "lucide-react";
import { updateMessagePart, deleteMessagePart } from "../../store";
import { sessionsCarburetor } from "../../state/sessions";
import { ConfirmDialog } from "../ConfirmDialog";
import { ForkSessionModal } from "../ForkSessionModal";
import { CopyButton } from "./CopyButton";
import { EditForm } from "./EditForm";
import { TextBlock } from "./TextBlock";

/** Props of the intermediate assistant message. */
interface IIntermediateAssistantMessageProps {
  messageID: string;
  partIndex: number;
  text: string;
  sessionID: string;
}

/** Local state of the intermediate assistant message. */
interface IIntermediateAssistantMessageState {
  editing: boolean;
  confirmDelete: boolean;
  forking: boolean;
  hovered: boolean;
}

/**
 * Renders a text part that was extracted from an otherwise-tool-bearing
 * assistant message (the model's running narrative between tool calls). It
 * behaves like a full assistant bubble: hover actions expose Copy, Fork, Edit,
 * and Delete. "Copy all" is intentionally absent because this is a sub-part of
 * a message, not an entire response.
 */
export class IntermediateAssistantMessage extends AntiHookComponent<
  IIntermediateAssistantMessageProps,
  IIntermediateAssistantMessageState
> {
  public state: IIntermediateAssistantMessageState = {
    editing: false,
    confirmDelete: false,
    forking: false,
    hovered: false,
  };

  /** Marks the row hovered. */
  @bind
  protected onMouseEnter(): void {
    this.setState({ hovered: true });
  }

  /** Marks the row not hovered. */
  @bind
  protected onMouseLeave(): void {
    this.setState({ hovered: false });
  }

  /** Opens the fork dialog. */
  @bind
  protected openFork(): void {
    this.setState({ forking: true });
  }

  /** Closes the fork dialog. */
  @bind
  protected closeFork(): void {
    this.setState({ forking: false });
  }

  /** Enters edit mode. */
  @bind
  protected openEdit(): void {
    this.setState({ editing: true });
  }

  /** Leaves edit mode without saving. */
  @bind
  protected closeEdit(): void {
    this.setState({ editing: false });
  }

  /** Opens the delete confirmation. */
  @bind
  protected openDelete(): void {
    this.setState({ confirmDelete: true });
  }

  /** Closes the delete confirmation. */
  @bind
  protected closeDelete(): void {
    this.setState({ confirmDelete: false });
  }

  /**
   * Persists a changed text and leaves edit mode.
   *
   * @param newText - the edited text
   */
  @bind
  protected handleSave(newText: string): void {
    const { messageID, partIndex, text } = this.props;
    if (newText && newText !== text) updateMessagePart(messageID, partIndex, newText);
    this.setState({ editing: false });
  }

  /** Deletes the part and closes the confirmation. */
  @bind
  protected handleConfirmDelete(): void {
    const { messageID, partIndex } = this.props;
    deleteMessagePart(messageID, partIndex);
    this.setState({ confirmDelete: false });
  }

  /** Renders the text (or edit form), the hover actions and the dialogs. */
  public render() {
    const { text, sessionID } = this.props;
    const { editing, confirmDelete, forking, hovered } = this.state;
    const { activeSessionID } = this.useCarburetor(sessionsCarburetor);
    const sid = sessionID || activeSessionID || "";

    return (
      <div
        className="msg-row flex flex-col px-5 py-3"
        onMouseEnter={this.onMouseEnter}
        onMouseLeave={this.onMouseLeave}
      >
        <div className="flex gap-3 justify-start">
          <div className="w-full min-w-0">
            {editing ? (
              <EditForm
                initialValue={text}
                rows={4}
                className="field-textarea text-[16px]"
                onSave={this.handleSave}
                onCancel={this.closeEdit}
              />
            ) : (
              <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
                <TextBlock text={text} isUser={false} />
              </div>
            )}
          </div>
        </div>

        {!editing && (
          <div className="msg-actions justify-start">
            {hovered && (
              <div className="flex items-center gap-1.5">
                <CopyButton text={text} />
                <button onClick={this.openFork} title="Fork session" className="btn-icon">
                  <GitFork size={13} />
                </button>
                <button onClick={this.openEdit} title="Edit" className="btn-icon">
                  <Pencil size={13} />
                </button>
                <button onClick={this.openDelete} title="Delete" className="btn-icon-danger">
                  <Trash2 size={13} />
                </button>
              </div>
            )}
          </div>
        )}

        {confirmDelete && (
          <ConfirmDialog
            title="Delete message part"
            message="This intermediate text from the agent will be removed. This cannot be undone."
            confirmLabel="Delete"
            onConfirm={this.handleConfirmDelete}
            onCancel={this.closeDelete}
          />
        )}

        {forking && sid && (
          <ForkSessionModal sessionID={sid} defaultTitle="" onClose={this.closeFork} />
        )}
      </div>
    );
  }
}
