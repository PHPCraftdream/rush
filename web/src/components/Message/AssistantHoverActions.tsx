// Hover action strip for assistant messages, incl. time/duration/usage badges.
// Pure code move from the former components/Message.tsx.

import { useCallback, memo } from "react";
import { Star, GitFork, Pencil, Trash2 } from "lucide-react";
import type { Message as Msg } from "../../types";
import { togglePinMessage } from "../../store";
import { CopyButton } from "./CopyButton";
import { CopyTurnButton } from "./CopyTurnButton";
import { TimeBadge } from "./TimeBadge";
import { DurationBadge } from "./DurationBadge";
import { UsageBadge } from "./UsageBadge";
import { EffortBadge } from "./EffortBadge";

export const AssistantHoverActions = memo(function AssistantHoverActions({
  message, copyText, onEdit, onDelete, onFork,
}: {
  message: Msg; copyText: string;
  onEdit: () => void; onDelete: () => void; onFork: () => void;
}) {
  const handlePin = useCallback(() => togglePinMessage(message.ID, !message.Pinned), [message.ID, message.Pinned]);
  return (
    <div className="flex items-center gap-1 w-full">
      <div className="flex items-center gap-1.5">
        {copyText && <CopyButton text={copyText} />}
        <CopyTurnButton messageID={message.ID} />
        <button onClick={handlePin} title={message.Pinned ? "Unpin" : "Pin message"} className={`p-1.5 transition-colors rounded ${message.Pinned ? "text-yellow" : "text-text-subtle hover:text-yellow"}`}><Star size={13} fill={message.Pinned ? "currentColor" : "none"} /></button>
        <button onClick={onFork}    title="Fork session" className="btn-icon"><GitFork size={13} /></button>
        <button onClick={onEdit}    title="Edit"         className="btn-icon"><Pencil  size={13} /></button>
        <button onClick={onDelete}  title="Delete"       className="btn-icon-danger"><Trash2 size={13} /></button>
      </div>
      <div className="flex items-center gap-2 ml-auto">
        <TimeBadge epochSec={message.CreatedAt} />
        <DurationBadge message={message} />
        <UsageBadge usage={message.Usage} />
        {message.Model && (
          <span className="text-xs text-text-subtle font-mono flex items-center gap-1">
            {message.Model}
            <EffortBadge effort={message.ReasoningEffort} />
          </span>
        )}
      </div>
    </div>
  );
});
