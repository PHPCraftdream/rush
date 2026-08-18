// Unified-diff renderer plus its single-pass parser.
// Pure code move from the former components/Message.tsx.

import { useMemo, memo } from "react";

// DiffLine — one rendered line of a unified diff. We do not require a real
// parser: a single pass over the string is enough to colour +/- lines and
// drop the noisy "+++"/"---" file headers.
type DiffLineKind = "add" | "del" | "ctx" | "hdr" | "meta";
interface DiffLine { kind: DiffLineKind; text: string }

function parseUnifiedDiff(diff: string): DiffLine[] {
  const out: DiffLine[] = [];
  const lines = diff.split(/\r?\n/);
  for (const line of lines) {
    if (line.startsWith("+++") || line.startsWith("---")) { out.push({ kind: "meta", text: line }); continue; }
    if (line.startsWith("@@")) { out.push({ kind: "hdr", text: line }); continue; }
    if (line.startsWith("+"))  { out.push({ kind: "add", text: line }); continue; }
    if (line.startsWith("-"))  { out.push({ kind: "del", text: line }); continue; }
    out.push({ kind: "ctx", text: line });
  }
  // Trim trailing blank line(s) — split adds an empty entry when diff ends with \n.
  while (out.length && out[out.length - 1].text === "" && out[out.length - 1].kind === "ctx") out.pop();
  return out;
}

export const DiffView = memo(function DiffView({ diff, additions, removals }: { diff: string; additions?: number; removals?: number }) {
  const lines = useMemo(() => parseUnifiedDiff(diff), [diff]);
  return (
    <div data-test-id="diff-view" className="diff-view">
      {(additions !== undefined || removals !== undefined) && (
        <div className="diff-stats text-xs mb-1">
          {additions !== undefined && <span className="text-green">+{additions}</span>}{" "}
          {removals !== undefined && <span className="text-red">−{removals}</span>}
        </div>
      )}
      <pre className="diff-body">
        {lines.map((l, i) => (
          <div key={i} className={`diff-line diff-${l.kind}`}>{l.text || " "}</div>
        ))}
      </pre>
    </div>
  );
});
