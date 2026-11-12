// Unified-diff renderer plus its single-pass parser.

import { AntiHookComponent } from "@react-carburetor/core";

// DiffLine — one rendered line of a unified diff. We do not require a real
// parser: a single pass over the string is enough to colour +/- lines and
// drop the noisy "+++"/"---" file headers.
type DiffLineKind = "add" | "del" | "ctx" | "hdr" | "meta";

/** One classified diff line. */
interface DiffLine {
  kind: DiffLineKind;
  text: string;
}

/** Props of the diff view. */
interface IDiffViewProps {
  diff: string;
  additions?: number;
  removals?: number;
}

/**
 * Classifies every line of a unified diff in a single pass.
 *
 * @param diff - the unified diff text
 */
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

/**
 * Renders one diff line.
 *
 * @param l - the classified line
 * @param i - the line index
 */
const renderDiffLine = (l: DiffLine, i: number) => (
  <div key={i} className={`diff-line diff-${l.kind}`}>{l.text || " "}</div>
);

/** Colours a unified diff, with optional +/- totals above it. */
export class DiffView extends AntiHookComponent<IDiffViewProps> {
  /** Renders the stats row and the coloured diff body. */
  public render() {
    const { diff, additions, removals } = this.props;
    const lines = parseUnifiedDiff(diff);
    return (
      <div data-test-id="diff-view" className="diff-view">
        {(additions !== undefined || removals !== undefined) && (
          <div className="diff-stats text-xs mb-1">
            {additions !== undefined && <span className="text-green">+{additions}</span>}{" "}
            {removals !== undefined && <span className="text-red">−{removals}</span>}
          </div>
        )}
        <pre className="diff-body">{lines.map(renderDiffLine)}</pre>
      </div>
    );
  }
}
