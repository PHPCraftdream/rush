// Renders one tool_result, with a coloured diff for file-write results.
// Pure code move from the former components/Message.tsx.

import { useState, memo } from "react";
import { CopyButton } from "./CopyButton";
import { DiffView } from "./DiffView";
import { FileWriteTools } from "./fileWriteTools";

interface WriteMetadata { diff?: string; additions?: number; removals?: number }

function safeParseWriteMetadata(raw: string | undefined): WriteMetadata | null {
  if (!raw) return null;
  try { return JSON.parse(raw) as WriteMetadata; } catch { return null; }
}

export const ToolResultBlock = memo(function ToolResultBlock({ name, content, isError, metadata }: { name: string; content: string; isError: boolean; metadata?: string }) {
  const isFileWrite = FileWriteTools.has(name);
  const meta        = isFileWrite ? safeParseWriteMetadata(metadata) : null;
  const hasDiff     = !!meta?.diff;
  const [diffOpen, setDiffOpen] = useState(true);

  return (
    <div data-test-id="tool-result" className="tool-block my-2 opacity-80">
      <div className="flex items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2">
          <span className="text-xs text-text-subtle">↩</span>
          <span className="text-text-muted font-semibold text-sm">{name}</span>
          {isError && <span data-test-id="tool-result-error" className="badge-error">error</span>}
        </div>
        <CopyButton text={hasDiff ? meta!.diff! : content} />
      </div>
      {hasDiff ? (
        <div className="tool-output-details">
          <button
            type="button"
            onClick={() => setDiffOpen((v) => !v)}
            aria-expanded={diffOpen}
            className="cursor-pointer text-text-subtle text-xs select-none bg-transparent border-0 p-0"
          >
            diff <span className="text-green">+{meta!.additions ?? 0}</span>{" "}
            <span className="text-red">−{meta!.removals ?? 0}</span>
          </button>
          {diffOpen && <DiffView diff={meta!.diff!} additions={meta!.additions} removals={meta!.removals} />}
        </div>
      ) : (
        <pre className="tool-output">{content}</pre>
      )}
    </div>
  );
});
