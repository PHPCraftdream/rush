// Collapsible context-condensation ("summary") message card.
// Pure code move from the former components/Message.tsx.

import { useState, useMemo, useCallback, memo } from "react";
import { BookMarked, Pencil } from "lucide-react";
import ReactMarkdown from "react-markdown";
import type { Message as Msg } from "../../types";
import { updateMessageContent } from "../../store";
import { DurationBadge } from "./DurationBadge";
import { EditForm } from "./EditForm";
import { EffortBadge } from "./EffortBadge";
import { MD_REMARK, MD_REHYPE } from "./mdPlugins";
import { useCollapseAllSignal } from "./useCollapseAllSignal";
import { extractText, isTerminallyFinished } from "./textParts";

// ── SummaryMessage ────────────────────────────────────────────────────────────

export const SummaryMessage = memo(function SummaryMessage({ message }: { message: Msg }) {
  const text = useMemo(() => extractText(message.Parts), [message.Parts]);
  const isFinished = useMemo(() => isTerminallyFinished(message.Parts), [message.Parts]);
  const [editing, setEditing] = useState(false);
  const [open, setOpen] = useState(false);
  useCollapseAllSignal(() => setOpen(false));

  const handleSave = useCallback((newText: string) => {
    if (newText && newText !== text) updateMessageContent(message.ID, newText);
    setEditing(false);
  }, [message.ID, text]);

  return (
    <div className="px-8 py-3">
      <div className="summary-card">
        <div className="summary-header">
          <BookMarked size={15} className="text-yellow shrink-0" />
          <span className="text-sm font-semibold text-yellow">Context condensed</span>
          <span className="ml-auto text-xs text-text-muted font-mono flex items-center gap-1">
            {message.Model}
            <EffortBadge effort={message.ReasoningEffort} />
          </span>
          {isFinished && <DurationBadge message={message} />}
          {isFinished && (
            <button onClick={() => setEditing(e => !e)} title="Edit summary" className="btn-icon-sm ml-1">
              <Pencil size={13} />
            </button>
          )}
        </div>
        <div className="group">
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            className="summary-toggle w-full text-left bg-transparent border-0"
          >
            {open ? "Hide summary ▾" : "Show summary ▸"}
          </button>
          {open && (editing ? (
            <div className="summary-body chat-font">
              <EditForm
                initialValue={text}
                rows={4}
                className="field-textarea"
                onSave={handleSave}
                onCancel={() => setEditing(false)}
              />
            </div>
          ) : text ? (
            <div className="summary-body md">
              <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
            </div>
          ) : null)}
        </div>
      </div>
    </div>
  );
});
