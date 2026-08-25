// The top-level chat message row: role switch, selection, hover actions.
// Pure code move from the former components/Message.tsx.

import { useState, useCallback, useMemo, memo } from "react";
import { Check, Star } from "lucide-react";
import { useStore } from "@nanostores/react";
import type { Message as Msg } from "../../types";
import { toggleMessageSelection, updateMessageContent, $busySessions } from "../../store";
import { ForkSessionModal } from "../ForkSessionModal";
import { AssistantContent } from "./AssistantContent";
import { AssistantHoverActions } from "./AssistantHoverActions";
import { BackgroundJobNotice } from "./BackgroundJobNotice";
import { SummaryMessage } from "./SummaryMessage";
import { UserContent } from "./UserContent";
import { UserHoverActions } from "./UserHoverActions";
import { extractText, isTerminallyFinished } from "./textParts";

// ── Message ───────────────────────────────────────────────────────────────────

export interface MessageProps {
  message: Msg;
  onDeleteRequest: (id: string) => void;
  onRerunRequest: (id: string) => void;
  onRangeSelect: (index: number) => void;
  selectionActive: boolean;
  isSelected: boolean;
  forkDefaultTitle: string;
  sessionID: string;
  index: number;
}

export const Message = memo(function Message({
  message, onDeleteRequest, onRerunRequest, onRangeSelect, selectionActive, isSelected, forkDefaultTitle, sessionID, index,
}: MessageProps) {
  if (message.Hidden) return null;
  if (message.IsSummaryMessage) return <SummaryMessage message={message} />;
  if (message.BackgroundJobNotice) return <BackgroundJobNotice message={message} />;

  const isUser = message.Role === "user";

  const copyText     = useMemo(() => extractText(message.Parts), [message.Parts]);
  const hasContent   = useMemo(() => !isUser && message.Parts.some(p => ["text","tool_call","tool_result","finish"].includes(p.type)), [isUser, message.Parts]);
  const busySessions = useStore($busySessions);

  // Orphan: an assistant message that is NOT terminally finished AND its
  // session is NOT busy. This represents a message from a crashed/killed turn
  // — the server's delete handler force-deletes it after proving the session
  // is idle via IsSessionBusy.
  const isOrphan = useMemo(
    () => !isUser && !isTerminallyFinished(message.Parts) && !busySessions.has(message.SessionID),
    [isUser, message.Parts, busySessions, message.SessionID]
  );

  // An assistant message with no terminal Finish part is still owned by an
  // in-flight agent turn. User messages are never streamed, so they are
  // always editable/deletable/selectable — this single flag gates all three
  // controls for an assistant message.
  //
  //   - Edit: the server refuses edits to a still-streaming message (task
  //     #590, handlers_messages.go's updateMessageAndVerify) because the
  //     turn's next checkpoint or terminal write would silently overwrite it.
  //   - Delete / selection-for-bulk-delete (task #595): the server now also
  //     refuses to delete a still-streaming assistant message
  //     (message.Service.Delete / DeleteMessageIfTerminal) because the live
  //     turn keeps writing to the same row after a delete would otherwise
  //     "resurrect" it in the UI (see docs/reviews/2026-08-19-release-
  //     readiness-static-follow-up-d3ee9841.md P1-1). Hiding Trash and the
  //     selection checkbox here means an operator never hits that refusal in
  //     the first place, and a still-streaming message can never end up
  //     inside a bulk delete_messages payload.
  //
  //   - Orphan exception: an unfinished assistant message in an IDLE session
  //     (i.e., the turn crashed or was killed) is considered an orphan and
  //     can be deleted/selected — the server's delete handler proves the
  //     session is idle via IsSessionBusy before force-deleting it. A BUSY
  //     session's live stream is still guarded.
  const streamGuardOK = useMemo(() => isUser || isTerminallyFinished(message.Parts), [isUser, message.Parts]);
  const editable      = streamGuardOK;
  const deletable     = streamGuardOK || isOrphan;
  const selectable    = streamGuardOK || isOrphan;

  const [editing, setEditing] = useState(false);
  const [forking, setForking] = useState(false);
  const [hovered, setHovered] = useState(false);

  const handleMouseEnter = useCallback(() => setHovered(true),  []);
  const handleMouseLeave = useCallback(() => setHovered(false), []);
  const handleForkOpen   = useCallback(() => setForking(true),  []);
  const handleForkClose  = useCallback(() => setForking(false), []);
  const handleEditOpen   = useCallback(() => setEditing(true),  []);
  const handleEditClose  = useCallback(() => setEditing(false), []);
  const handleDelete     = useCallback(() => onDeleteRequest(message.ID), [onDeleteRequest, message.ID]);
  const handleRerun      = useCallback(() => onRerunRequest(message.ID), [onRerunRequest, message.ID]);

  const handleSaveEdit = useCallback((text: string) => {
    if (text && text !== extractText(message.Parts)) updateMessageContent(message.ID, text);
    setEditing(false);
  }, [message.ID, message.Parts]);

  const handleCheckboxClick = useCallback((e: React.MouseEvent) => {
    // task #595: a still-streaming assistant message can never be selected,
    // so it can never end up inside a delete_messages bulk payload — see
    // streamGuardOK above. This mirrors "hide/disable Trash and selection-
    // delete until the message is terminally finished" from the P1-1 fix:
    // selection is the OTHER path that reaches bulk delete besides the
    // single Trash button.
    //
    // Orphan exception: an unfinished assistant message in an IDLE session
    // (i.e., the turn crashed or was killed) can be selected for deletion —
    // the server proves the session is idle before force-deleting.
    if (!selectable) return;
    if (e.shiftKey) {
      e.preventDefault(); // Prevent text selection between clicks
      onRangeSelect(index);
    }
    else { toggleMessageSelection(message.ID); }
  }, [message.ID, index, onRangeSelect, selectable]);

  // Checkbox is always in DOM (reserves layout space), opacity-0 when not
  // relevant. A non-selectable (still-streaming) message never shows it,
  // regardless of hover/selection-mode state — there is nothing useful the
  // control could do (see handleCheckboxClick's early return above).
  // Orphan exception: an unfinished assistant message in an IDLE session
  // (i.e., the turn crashed or was killed) can be selected for deletion.
  const checkboxVisible = selectable && (selectionActive || isSelected || hovered);

  return (
    <div
      id={`msg-${message.ID}`}
      data-msg-role={isUser ? "user" : "assistant"}
      className={`msg-row flex flex-col px-5 py-3 transition-colors ${isSelected ? "bg-accent/5" : ""} ${message.Pinned ? "border-l-4 border-yellow/60 bg-yellow/[5%]" : ""}`}
      onMouseEnter={handleMouseEnter}
      onMouseLeave={handleMouseLeave}
    >
      <div className={`flex gap-3 ${isUser ? "justify-end" : "justify-start"}`}>
        <div
          className={`msg-checkbox-wrap ${checkboxVisible ? "opacity-100" : "opacity-0 pointer-events-none"}`}
          style={{ order: -1 }}
          onClick={handleCheckboxClick}
        >
          <div className={`msg-checkbox ${isSelected ? "bg-accent border-accent" : "border-text-subtle/50 hover:border-accent"}`}>
            {isSelected && <Check size={10} className="text-white shrink-0" />}
          </div>
        </div>
        {isUser ? (
          <div className="max-w-[80%]">
            <UserContent message={message} editing={editing} onSaveEdit={handleSaveEdit} onCancelEdit={handleEditClose} />
          </div>
        ) : (
          <div className="w-full min-w-0">
            <AssistantContent message={message} editing={editing} onSaveEdit={handleSaveEdit} onCancelEdit={handleEditClose} />
          </div>
        )}
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
          {hovered && (
            isUser ? (
              <UserHoverActions
                messageID={message.ID}
                copyText={copyText}
                isPinned={message.Pinned}
                onEdit={handleEditOpen}
                onDelete={handleDelete}
                onFork={handleForkOpen}
                onRerun={handleRerun}
              />
            ) : hasContent ? (
              <AssistantHoverActions
                message={message}
                copyText={copyText}
                editable={editable}
                deletable={deletable}
                onEdit={handleEditOpen}
                onDelete={handleDelete}
                onFork={handleForkOpen}
              />
            ) : null
          )}
        </div>
      )}

      {forking && (
        <ForkSessionModal sessionID={sessionID} defaultTitle={forkDefaultTitle} onClose={handleForkClose} />
      )}
    </div>
  );
});
