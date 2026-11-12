// Hover action strip for user messages (copy, rerun, pin, fork, edit, delete).
// Only mounted while the message is hovered.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import { RotateCcw, Star, GitFork, Pencil, Trash2 } from "lucide-react";
import { togglePinMessage } from "../../store";
import { CopyButton } from "./CopyButton";
import { CopyTurnButton } from "./CopyTurnButton";

/** Props of the user hover action strip. */
interface IUserHoverActionsProps {
  messageID: string;
  copyText: string;
  isPinned: boolean;
  onEdit: () => void;
  onDelete: () => void;
  onFork: () => void;
  onRerun: () => void;
}

/** Copy, retry, pin, fork, edit and delete controls of a user message. */
export class UserHoverActions extends AntiHookComponent<IUserHoverActionsProps> {
  /** Toggles the pinned flag of the message. */
  @bind
  protected handlePin(): void {
    togglePinMessage(this.props.messageID, !this.props.isPinned);
  }

  /** Renders the strip. */
  public render() {
    const { copyText, isPinned, onEdit, onDelete, onFork, onRerun, messageID } = this.props;
    const pinTone = isPinned ? "text-yellow" : "text-text-subtle hover:text-yellow";
    return (
      <div className="flex items-center gap-1.5">
        {copyText && <CopyButton text={copyText} />}
        <CopyTurnButton messageID={messageID} />
        <button onClick={onRerun} title="Retry" className="btn-icon"><RotateCcw size={13} /></button>
        <button
          onClick={this.handlePin}
          title={isPinned ? "Unpin" : "Pin message"}
          className={`p-1.5 transition-colors rounded ${pinTone}`}
        >
          <Star size={13} fill={isPinned ? "currentColor" : "none"} />
        </button>
        <button onClick={onFork} title="Fork session" className="btn-icon"><GitFork size={13} /></button>
        <button onClick={onEdit} title="Edit" className="btn-icon"><Pencil size={13} /></button>
        <button onClick={onDelete} title="Delete" className="btn-icon-danger"><Trash2 size={13} /></button>
      </div>
    );
  }
}
