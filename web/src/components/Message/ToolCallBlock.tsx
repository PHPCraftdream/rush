// Renders one tool_call: header, pretty-printed input, edit previews for
// file writes. Pure code move from the former components/Message.tsx.

import { useState, useMemo, memo } from "react";
import { CopyButton } from "./CopyButton";
import { EditPreview } from "./EditPreview";
import { FileWriteTools } from "./fileWriteTools";

function formatJSON(s: string) {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

// prettyToolInput renders a tool-call argument object so multiline string
// values (a bash heredoc, a grep pattern, a fetched body) keep their REAL
// line breaks and indentation instead of being flattened into a single
// `"command": "…\n…\n…"` JSON line where every newline shows as a literal
// `\n`. Flat objects render as `key: value`, with multiline strings broken
// onto their own lines; nested objects/arrays (e.g. multiedit's edits) fall
// back to indented JSON so structure is never lost. Non-object inputs go
// through formatJSON unchanged.
function prettyToolInput(input: string): string {
  let parsed: unknown;
  try { parsed = JSON.parse(input); } catch { return input; }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return formatJSON(input);
  }
  const out: string[] = [];
  for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
    if (typeof v === "object" && v !== null) {
      out.push(`${k}: ${JSON.stringify(v, null, 2)}`);
    } else if (typeof v === "string" && v.includes("\n")) {
      out.push(`${k}:`);
      out.push(v.replace(/\s+$/, ""));
      out.push("");
    } else if (typeof v === "string") {
      out.push(`${k}: ${v}`);
    } else {
      out.push(`${k}: ${JSON.stringify(v)}`);
    }
  }
  return out.join("\n").replace(/\n+$/, "");
}

interface WriteInput { file_path?: string; content?: string; old_string?: string; new_string?: string }

function safeParseWriteInput(raw: string): WriteInput {
  try { return JSON.parse(raw) as WriteInput; } catch { return {}; }
}

// `running` is caller-supplied truth ("call exists, no matching
// tool_result yet"). ToolCall.Finished must not be used for this badge:
// it means the model finished typing the arguments and flips true before
// the tool is dispatched (internal/agent/agent_turn.go), which showed
// "running" for a blink at the wrong moment and hid it while the tool
// actually executed.
export const ToolCallBlock = memo(function ToolCallBlock({ name, input, running }: { name: string; input: string; running?: boolean }) {
  const isFileWrite = FileWriteTools.has(name);
  const writeInput  = isFileWrite ? safeParseWriteInput(input) : null;
  const formatted   = useMemo(() => prettyToolInput(input), [input]);
  const [rawOpen, setRawOpen] = useState(false);

  return (
    <div data-test-id="tool-call" className="tool-block my-2">
      <div className="flex items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2">
          <span className="text-xs text-text-subtle">⚡</span>
          <span className="text-mauve font-semibold text-sm">{name}</span>
          {writeInput?.file_path && <span className="text-text-muted text-xs font-mono truncate max-w-[36em]">{writeInput.file_path}</span>}
          {running && <span data-test-id="tool-call-running" className="text-text-subtle text-xs animate-pulse">running…</span>}
        </div>
        <CopyButton text={input} />
      </div>
      {isFileWrite && writeInput?.old_string != null ? (
        // edit / multiedit: show old→new as a mini inline diff
        <div className="tool-output-details">
          <EditPreview old={writeInput.old_string} new_={writeInput.new_string ?? ""} />
          <button
            type="button"
            onClick={() => setRawOpen((v) => !v)}
            aria-expanded={rawOpen}
            className="cursor-pointer text-text-subtle text-xs select-none bg-transparent border-0 p-0 mt-1"
          >
            {rawOpen ? "hide raw input" : "show raw input"}
          </button>
          {rawOpen && <pre className="tool-output mt-1">{formatted}</pre>}
        </div>
      ) : isFileWrite ? (
        <div className="tool-output-details">
          {writeInput?.content && (
            <pre className="tool-output mb-1 max-h-40 overflow-y-auto">{writeInput.content.length > 2000 ? writeInput.content.slice(0, 2000) + "\n…" : writeInput.content}</pre>
          )}
          <button
            type="button"
            onClick={() => setRawOpen((v) => !v)}
            aria-expanded={rawOpen}
            className="cursor-pointer text-text-subtle text-xs select-none bg-transparent border-0 p-0"
          >
            {rawOpen ? "hide raw input" : "show raw input"}
          </button>
          {rawOpen && <pre className="tool-output mt-1">{formatted}</pre>}
        </div>
      ) : (
        <pre className="tool-output">{formatted}</pre>
      )}
    </div>
  );
});
