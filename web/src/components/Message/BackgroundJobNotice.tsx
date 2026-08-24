// Left-aligned system notice for background-job results.
// Pure code move from the former components/Message.tsx.

import { useState, useMemo, memo } from "react";
import ReactMarkdown from "react-markdown";
import type { Message as Msg } from "../../types";
import { MD_REMARK, MD_REHYPE } from "./mdPlugins";
import { useCollapseAllSignal } from "./useCollapseAllSignal";
import { extractText } from "./textParts";

// ── BackgroundJobNotice ───────────────────────────────────────────────────────
// Background-job notices are persisted as Role:"user" (the model must react to
// the job's result) but the operator never typed them, so they render on the
// LEFT as a muted system notice — mirroring SummaryMessage's container idiom.

export const BackgroundJobNotice = memo(function BackgroundJobNotice({ message }: { message: Msg }) {
  const text = useMemo(() => extractText(message.Parts), [message.Parts]);
  // Collapsed by default — orchestrator output is noise the operator only
  // occasionally needs to inspect, so it folds into a spoiler like every
  // other orchestrator block (mirrors SummaryMessage's toggle).
  const [open, setOpen] = useState(false);
  useCollapseAllSignal(() => setOpen(false));
  return (
    <div className="px-8 py-3">
      <div className="summary-card">
        <div className="summary-header">
          <span
            className="px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px]"
            title="background job finished — injected by rush, not typed by you"
          >
            ⚙ background job
          </span>
          {message.AutoResumed && (
            <span
              className="px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px]"
              title="auto-resumed: background job finished"
            >
              ↻ auto-resumed
            </span>
          )}
        </div>
        {text ? (
          <div className="group">
            <button
              type="button"
              onClick={() => setOpen((v) => !v)}
              aria-expanded={open}
              className="summary-toggle w-full text-left bg-transparent border-0"
            >
              {open ? "Hide output ▾" : "Show output ▸"}
            </button>
            {open && (
              <div className="summary-body md">
                <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
              </div>
            )}
          </div>
        ) : null}
      </div>
    </div>
  );
});
