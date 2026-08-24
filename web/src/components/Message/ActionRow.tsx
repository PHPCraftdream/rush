// One accordion row inside a tool activity group (a tool call/result pair
// or a thinking part). Pure code move from the former components/Message.tsx.

import { useState, useCallback, memo } from "react";
import { BrainCircuit, Pencil, Trash2, ChevronDown, ChevronUp } from "lucide-react";
import type { ContentPart } from "../../types";
import { updateMessagePart, deleteMessagePart } from "../../store";
import { ConfirmDialog } from "../ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { EditForm } from "./EditForm";
import { EffortBadge } from "./EffortBadge";
import { TimeBadge } from "./TimeBadge";
import { ToolCallBlock } from "./ToolCallBlock";
import { ToolResultBlock } from "./ToolResultBlock";

// formatActionArgs — one-line preview shown in the collapsed row header.
// Picks the most useful identifying argument for known tool names so the
// reader scans by file path / command / pattern, not by raw JSON.
function formatActionArgs(name: string, input: string): string {
  if (!input) return "";
  let parsed: Record<string, unknown> = {};
  try { parsed = JSON.parse(input) as Record<string, unknown>; } catch { return ""; }
  const s = (k: string) => typeof parsed[k] === "string" ? (parsed[k] as string) : "";
  switch (name) {
    case "bash":      return s("command");
    case "view":      return s("file_path") || s("path") || s("filePath");
    case "write":
    case "edit":
    case "multiedit": return s("file_path");
    case "glob":      return s("pattern");
    case "grep":      return [s("pattern"), s("path")].filter(Boolean).join(" · ");
    case "ls":        return s("path");
    case "fetch":     return s("url");
    case "download":  return s("url");
    case "agent":     return s("prompt") || s("description");
    default: {
      // First string value in the object as a sensible fallback.
      for (const v of Object.values(parsed)) if (typeof v === "string" && v) return v;
      return "";
    }
  }
}

export type ActionItem =
  | {
      kind: "tool";
      callPart?: ContentPart & { type: "tool_call"; ID: string; Name: string; Input: string; Finished: boolean };
      resultPart?: ContentPart & { type: "tool_result"; ToolCallID: string; Name: string; Content: string; IsError: boolean; Metadata?: string };
      idx: number;
      key: string;
      createdAt?: number;
      repeatCount?: number;
      model?: string;
      effort?: string;
    }
  | {
      kind: "thinking";
      text: string;
      idx: number;
      key: string;
      createdAt?: number;
      messageID?: string;
      partIndex: number;
      model?: string;
      effort?: string;
    };

interface ActionRowProps {
  item: ActionItem;
  isCurrent: boolean;
  suppressAutoCurrent: boolean;
  model?: string;
  effort?: string;
}

export const ActionRow = memo(function ActionRow({ item, isCurrent, suppressAutoCurrent, model, effort }: ActionRowProps) {
  // override:
  //   undefined → follow auto-rule (open iff isCurrent, AND auto isn't suppressed)
  //   true / false → user pinned, ignore auto-rule from now on
  const [override, setOverride] = useState<boolean | undefined>(undefined);
  const effectiveCurrent = suppressAutoCurrent ? false : isCurrent;
  const open = override ?? effectiveCurrent;
  const toggle = useCallback(() => setOverride(!open), [open]);

  // Used only by the thinking branch; useState must be called unconditionally.
  const [editingThinking, setEditingThinking] = useState(false);
  const [confirmDeleteThinking, setConfirmDeleteThinking] = useState(false);

  if (item.kind === "thinking") {
    // Thinking rows live alongside tool rows in the accordion. Same
    // open/close + auto-current rules; collapsed header shows a one-line
    // preview of the model's reasoning so the operator can scan the
    // chain without expanding every row.
    const preview = item.text.replace(/\s+/g, " ").trim();
    const messageID = item.messageID ?? "";
    const partIndex = item.partIndex ?? -1;
    return (
      <div data-test-id="action-row" className="action-row group">
        <button
          type="button"
          onClick={toggle}
          aria-expanded={open}
          data-test-id="action-row-toggle"
          className="action-row-head"
          title={preview || "thinking"}
        >
          <span className="text-accent/70 shrink-0"><BrainCircuit size={13} /></span>
          <span className="text-accent/80 font-semibold text-sm shrink-0">thinking</span>
          {model && <span className="text-xs text-text-subtle font-mono shrink-0">{model}</span>}
          <EffortBadge effort={effort} extraClass="shrink-0" />
          <span className="text-text font-mono text-sm truncate flex-1 min-w-0">
            {preview || "—"}
          </span>
          <TimeBadge epochSec={item.createdAt} />
          {messageID && partIndex >= 0 && (
            <div className="flex items-center gap-0.5 hover-reveal shrink-0" onClick={(e) => e.stopPropagation()}>
              <CopyButton text={item.text} className="px-1.5 py-1 text-xs" />
              <button
                onClick={(e) => { e.preventDefault(); e.stopPropagation(); setEditingThinking(true); }}
                title="Edit thinking"
                className="btn-icon-sm"
              >
                <Pencil size={13} />
              </button>
              <button
                onClick={(e) => { e.preventDefault(); e.stopPropagation(); setConfirmDeleteThinking(true); }}
                title="Delete thinking"
                className="btn-icon-sm-danger"
              >
                <Trash2 size={13} />
              </button>
            </div>
          )}
          <span className="text-text-subtle shrink-0">
            {open ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
          </span>
        </button>
        {open && (
          <div className="action-row-body">
            {editingThinking ? (
              <div className="p-4 bg-base-overlay border-t border-surface">
                <EditForm
                  initialValue={item.text}
                  rows={6}
                  className="w-full bg-base-subtle border border-accent/40 text-text-muted rounded-lg px-4 py-3 text-[14px] font-mono leading-relaxed resize-none outline-none focus:border-accent"
                  onSave={(t) => { updateMessagePart(messageID, partIndex, t); setEditingThinking(false); }}
                  onCancel={() => setEditingThinking(false)}
                />
              </div>
            ) : (
              <pre className="tool-output whitespace-pre-wrap">{item.text}</pre>
            )}
          </div>
        )}
        {confirmDeleteThinking && (
          <ConfirmDialog
            title="Delete thinking"
            message="The model's reasoning will be removed from this message. This cannot be undone."
            confirmLabel="Delete"
            onConfirm={() => { deleteMessagePart(messageID, partIndex); setConfirmDeleteThinking(false); }}
            onCancel={() => setConfirmDeleteThinking(false)}
          />
        )}
      </div>
    );
  }

  const call    = item.callPart;
  const result  = item.resultPart;
  const name    = call?.Name ?? result?.Name ?? "tool";
  const subject = call ? formatActionArgs(call.Name, call.Input) : "";
  // `result` (paired by ToolCallID) is the real "tool returned" signal.
  // call.Finished is NOT: it means the model finished typing the
  // arguments and is set before the tool is dispatched
  // (internal/agent/agent_turn.go OnToolInputEnd/OnToolCall), so keying
  // on it flashed the badge for a blink while args streamed and hid it
  // for the whole execution window.
  const running = !!call && !result;
  const errored = !!result?.IsError;
  return (
    <div data-test-id="action-row" className="action-row">
      <button
        type="button"
        onClick={toggle}
        aria-expanded={open}
        data-test-id="action-row-toggle"
        className="action-row-head"
        title={subject || name}
      >
        <span className="text-xs text-text-subtle shrink-0">⚡</span>
        <span className="text-mauve font-semibold text-sm shrink-0">{name}</span>
        {/* Subject (file path, command, pattern, …) is the primary readable
            label of the row — same size and weight as the tool name so a
            collapsed accordion immediately tells the operator WHICH file
            each action touched, not just that "an edit happened". */}
        <span className="text-text font-mono text-sm truncate flex-1 min-w-0">
          {subject || "—"}
        </span>
        {item.repeatCount && item.repeatCount > 1 && <span className="px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px] shrink-0">×{item.repeatCount}</span>}
        {running && <span className="text-text-subtle text-xs animate-pulse shrink-0">running…</span>}
        {errored && <span className="badge-error shrink-0">error</span>}
        <TimeBadge epochSec={item.createdAt} />
        <span className="text-text-subtle shrink-0">
          {open ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
        </span>
      </button>
      {open && (
        <div className="action-row-body">
          {call && <ToolCallBlock name={call.Name} input={call.Input} running={running} />}
          {result && <ToolResultBlock name={result.Name} content={result.Content} isError={result.IsError} metadata={result.Metadata} />}
        </div>
      )}
    </div>
  );
});
