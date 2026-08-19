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
  message, copyText, editable, deletable, onEdit, onDelete, onFork,
}: {
  message: Msg; copyText: string; editable: boolean; deletable: boolean;
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
        {editable && (
          // Hidden (not just disabled) while the message is still
          // mid-stream: the server refuses this edit anyway (task #590 —
          // an edit made before the turn's terminal write would be
          // silently overwritten), so there is nothing useful the control
          // could do here.
          <button onClick={onEdit} title="Edit" className="btn-icon"><Pencil size={13} /></button>
        )}
        {deletable && (
          // Hidden (not just disabled) while the message is still
          // mid-stream in a BUSY session: the server refuses this delete
          // (task #595 — message.Service.Delete / DeleteMessageIfTerminal),
          // because the live turn's own terminal write to this same row
          // would otherwise race a delete and "resurrect" the message in
          // the UI. Same rationale as the Edit gate above.
          //
          // Orphan exception: an unfinished assistant message in an IDLE
          // session (i.e., the turn crashed or was killed) can be deleted —
          // the server proves the session is idle before force-deleting it.
          // The orphan detection is done in Message.tsx's deletable prop.
          <button onClick={onDelete} title="Delete" className="btn-icon-danger"><Trash2 size={13} /></button>
        )}
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
