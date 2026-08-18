// Hover action strip for user messages (copy, rerun, pin, fork, edit, delete).
// Pure code move from the former components/Message.tsx.

import { useCallback, memo } from "react";
import { RotateCcw, Star, GitFork, Pencil, Trash2 } from "lucide-react";
import { rerunFromMessage, togglePinMessage } from "../../store";
import { CopyButton } from "./CopyButton";
import { CopyTurnButton } from "./CopyTurnButton";

// ── Hover action strips — only mounted when hovered ───────────────────────────

export const UserHoverActions = memo(function UserHoverActions({
  messageID, copyText, isLastUserMsg, isPinned, onEdit, onDelete, onFork,
}: {
  messageID: string; copyText: string; isLastUserMsg: boolean; isPinned: boolean;
  onEdit: () => void; onDelete: () => void; onFork: () => void;
}) {
  const handleRerun = useCallback(() => rerunFromMessage(messageID), [messageID]);
  const handlePin   = useCallback(() => togglePinMessage(messageID, !isPinned), [messageID, isPinned]);
  return (
    <div className="flex items-center gap-1.5">
      {copyText && <CopyButton text={copyText} />}
      <CopyTurnButton messageID={messageID} />
      {isLastUserMsg && <button onClick={handleRerun} title="Rerun" className="btn-icon"><RotateCcw size={13} /></button>}
      <button onClick={handlePin}   title={isPinned ? "Unpin" : "Pin message"} className={`p-1.5 transition-colors rounded ${isPinned ? "text-yellow" : "text-text-subtle hover:text-yellow"}`}><Star size={13} fill={isPinned ? "currentColor" : "none"} /></button>
      <button onClick={onFork}      title="Fork session"                       className="btn-icon"><GitFork size={13} /></button>
      <button onClick={onEdit}      title="Edit"                               className="btn-icon"><Pencil  size={13} /></button>
      <button onClick={onDelete}    title="Delete"                             className="btn-icon-danger"><Trash2 size={13} /></button>
    </div>
  );
});
