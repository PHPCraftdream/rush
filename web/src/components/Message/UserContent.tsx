// User-message content area (bubble or edit form).
// Pure code move from the former components/Message.tsx.

import { memo } from "react";
import type { Message as Msg } from "../../types";
import { EditForm } from "./EditForm";
import { Part } from "./Part";
import { extractText } from "./textParts";

// ── Message content areas — own editing state ─────────────────────────────────

export const UserContent = memo(function UserContent({
  message, editing, onSaveEdit, onCancelEdit,
}: {
  message: Msg;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}) {
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
      {message.Parts.map((part, i) => <Part key={i} part={part} index={i} isUser messageID={message.ID} thinkingDone={false} sessionID={message.SessionID} />)}
    </div>
  );
});
