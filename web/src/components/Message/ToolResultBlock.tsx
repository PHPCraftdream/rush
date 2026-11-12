// Renders one tool_result, with a coloured diff for file-write results.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import { CopyButton } from "./CopyButton";
import { DiffView } from "./DiffView";
import { FileWriteTools } from "./fileWriteTools";

/** The diff fields a file-writing tool's result metadata carries. */
interface WriteMetadata {
  diff?: string;
  additions?: number;
  removals?: number;
}

/**
 * Parses a file-writing tool's result metadata; null when absent or invalid.
 *
 * @param raw - the raw metadata JSON
 */
function safeParseWriteMetadata(raw: string | undefined): WriteMetadata | null {
  if (!raw) return null;
  try { return JSON.parse(raw) as WriteMetadata; } catch { return null; }
}

/** Props of the tool-result block. */
interface IToolResultBlockProps {
  name: string;
  content: string;
  isError: boolean;
  metadata?: string;
}

/** Local state of the tool-result block. */
interface IToolResultBlockState {
  diffOpen: boolean;
}

/** One tool result: raw output, or a collapsible coloured diff for file writes. */
export class ToolResultBlock extends AntiHookComponent<IToolResultBlockProps, IToolResultBlockState> {
  public state: IToolResultBlockState = { diffOpen: true };

  /** Flips the diff visibility. */
  @bind
  protected toggleDiff(): void {
    this.setState({ diffOpen: !this.state.diffOpen });
  }

  /** Renders the header and the output or diff. */
  public render() {
    const { name, content, isError, metadata } = this.props;
    const { diffOpen } = this.state;
    const meta = FileWriteTools.has(name) ? safeParseWriteMetadata(metadata) : null;
    const diff = meta?.diff;

    return (
      <div data-test-id="tool-result" className="tool-block my-2 opacity-80">
        <div className="flex items-center justify-between gap-2 mb-2">
          <div className="flex items-center gap-2">
            <span className="text-xs text-text-subtle">↩</span>
            <span className="text-text-muted font-semibold text-sm">{name}</span>
            {isError && <span data-test-id="tool-result-error" className="badge-error">error</span>}
          </div>
          <CopyButton text={diff ? diff : content} />
        </div>
        {diff ? (
          <div className="tool-output-details">
            <button
              type="button"
              onClick={this.toggleDiff}
              aria-expanded={diffOpen}
              className="cursor-pointer text-text-subtle text-xs select-none bg-transparent border-0 p-0"
            >
              diff <span className="text-green">+{meta?.additions ?? 0}</span>{" "}
              <span className="text-red">−{meta?.removals ?? 0}</span>
            </button>
            {diffOpen && <DiffView diff={diff} additions={meta?.additions} removals={meta?.removals} />}
          </div>
        ) : (
          <pre className="tool-output">{content}</pre>
        )}
      </div>
    );
  }
}
