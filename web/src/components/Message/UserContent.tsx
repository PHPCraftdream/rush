// User-message content area (bubble or edit form).

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { Message as Msg, ContentPart } from "../../types";
import { EditForm } from "./EditForm";
import { Part } from "./Part";
import { extractText } from "./extractText";

/** Props of the user-message content area. */
interface IUserContentProps {
  message: Msg;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}

/** User-message content: the bubble, or the edit form while editing. */
export class UserContent extends AntiHookComponent<IUserContentProps> {
  /**
   * Renders one part of the bubble.
   *
   * @param part - the message part
   * @param i - the part index
   */
  @bind
  protected renderPart(part: ContentPart, i: number) {
    const { message } = this.props;
    return (
      <Part
        key={i}
        part={part}
        index={i}
        isUser
        messageID={message.ID}
        thinkingDone={false}
        partialWorkDone={false}
        sessionID={message.SessionID}
      />
    );
  }

  /** Renders the edit form or the bubble. */
  public render() {
    const { message, editing, onSaveEdit, onCancelEdit } = this.props;
    if (editing) {
      return (
        <EditForm
          initialValue={extractText(message.Parts)}
          rows={1}
          className="msg-bubble-user-edit text-[18px] min-w-[300px]"
          onSave={onSaveEdit}
          onCancel={onCancelEdit}
        />
      );
    }
    return (
      <div className="msg-bubble-user" style={{ fontSize: "var(--chat-font-size)" }}>
        {message.AutoResumed && (
          <span
            className="px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px] mb-1 inline-block"
            title="auto-resumed: background job finished"
          >
            ↻ auto-resumed
          </span>
        )}
        {message.Parts.map(this.renderPart)}
      </div>
    );
  }
}
