// Renders an intermediate text part extracted from a tool-bearing assistant
// message. Pure code move from the former components/Message.tsx.

import { useState, useCallback } from "react";
import { useStore } from "@nanostores/react";
import { GitFork, Pencil, Trash2 } from "lucide-react";
import { $activeSessionID, updateMessagePart, deleteMessagePart } from "../../store";
import { ConfirmDialog } from "../ConfirmDialog";
import { ForkSessionModal } from "../ForkSessionModal";
import { CopyButton } from "./CopyButton";
import { EditForm } from "./EditForm";
import { TextBlock } from "./TextBlock";

// IntermediateAssistantMessage renders a text part that was extracted from
// an otherwise-tool-bearing assistant message (the model's running narrative
// between tool calls). It behaves like a full assistant bubble: hover actions
// expose Copy, Fork, Edit, and Delete. "Copy all" is intentionally absent
// because this is a sub-part of a message, not an entire response.
export function IntermediateAssistantMessage({
  messageID,
  partIndex,
  text,
  sessionID,
}: {
  messageID: string;
  partIndex: number;
  text: string;
  sessionID: string;
}) {
  const [editing, setEditing] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [forking, setForking] = useState(false);
  const [hovered, setHovered] = useState(false);

  const activeSessionID = useStore($activeSessionID);

  const handleSave = useCallback(
    (newText: string) => {
      if (newText && newText !== text) updateMessagePart(messageID, partIndex, newText);
      setEditing(false);
    },
    [messageID, partIndex, text],
  );

  const handleConfirmDelete = useCallback(() => {
    deleteMessagePart(messageID, partIndex);
    setConfirmDelete(false);
  }, [messageID, partIndex]);

  const sid = sessionID || activeSessionID || "";

  return (
    <div
      className="msg-row flex flex-col px-5 py-3"
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
    >
      <div className="flex gap-3 justify-start">
        <div className="w-full min-w-0">
          {editing ? (
            <EditForm
              initialValue={text}
              rows={4}
              className="field-textarea text-[16px]"
              onSave={handleSave}
              onCancel={() => setEditing(false)}
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
              <button
                onClick={() => setForking(true)}
                title="Fork session"
                className="btn-icon"
              >
                <GitFork size={13} />
              </button>
              <button
                onClick={() => setEditing(true)}
                title="Edit"
                className="btn-icon"
              >
                <Pencil size={13} />
              </button>
              <button
                onClick={() => setConfirmDelete(true)}
                title="Delete"
                className="btn-icon-danger"
              >
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
          onConfirm={handleConfirmDelete}
          onCancel={() => setConfirmDelete(false)}
        />
      )}

      {forking && sid && (
        <ForkSessionModal
          sessionID={sid}
          defaultTitle=""
          onClose={() => setForking(false)}
        />
      )}
    </div>
  );
}
