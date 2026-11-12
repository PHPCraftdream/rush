import { createRef } from "react";
import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { CornerLeftUp } from "lucide-react";
import type { MyPromptItem } from "../../store";
import { myPrompts } from "../../state/myPrompts";

/** Props of one history row. */
interface IHistoryRowProps {
  item: MyPromptItem;
  selected: boolean;
  onRecall: (item: MyPromptItem) => void;
  onJump: (item: MyPromptItem) => void;
}

/** One prompt of the history menu: the body recalls it, the arrow jumps to it. */
class HistoryRow extends AntiHookComponent<IHistoryRowProps> {
  /**
   * Puts the prompt back in the input without stealing focus.
   *
   * @param e - the mousedown event
   */
  @bind
  protected recall(e: MouseEvent<HTMLButtonElement>): void {
    e.preventDefault();
    this.props.onRecall(this.props.item);
  }

  /**
   * Jumps the transcript to the prompt without stealing focus.
   *
   * @param e - the mousedown event
   */
  @bind
  protected jump(e: MouseEvent<HTMLButtonElement>): void {
    e.preventDefault();
    this.props.onJump(this.props.item);
  }

  /** Renders the row. */
  public render() {
    const tone = this.props.selected
      ? "bg-accent/20 border-l-2 border-l-accent"
      : "hover:bg-base-subtle border-l-2 border-l-transparent";
    return (
      <div
        className={`group/row flex items-stretch border-b border-surface/50 last:border-0 transition-colors ${tone}`}
      >
        <button
          onMouseDown={this.recall}
          className="flex-1 text-left px-4 py-2.5 min-w-0"
          title="Click to put this prompt back in the input"
        >
          <div className="text-text text-sm whitespace-pre-wrap line-clamp-3 break-words font-medium">
            {this.props.item.text}
          </div>
        </button>
        <button
          onMouseDown={this.jump}
          title="Jump to this message in the transcript"
          className={
            "flex items-center justify-center px-3 text-text-subtle hover:text-accent border-l " +
            "border-surface/50 transition-colors"
          }
        >
          <CornerLeftUp size={16} />
        </button>
      </div>
    );
  }
}

/** Props of the history menu. */
interface IHistoryMenuProps {
  selectedIdx: number;
  onRecall: (item: MyPromptItem) => void;
  onJump: (item: MyPromptItem) => void;
}

/** Classes of the dropdown shell, shared by the list and the empty notice. */
const SHELL_CLASS =
  "absolute bottom-full left-0 right-0 mb-2 bg-base-overlay border border-surface rounded-2xl shadow-2xl " +
  "overflow-hidden z-50";

/**
 * History menu — recall + jump for the user's own prompts.
 *
 * Two-action row: clicking the body fills the textarea (recall, for editing
 * or re-sending); clicking the arrow icon jumps the transcript to that
 * message (preserves the current draft, just navigates). Up/Down to walk
 * the list, Enter to recall the selected row, Esc to close.
 */
export class HistoryMenu extends AntiHookComponent<IHistoryMenuProps> {
  private readonly listRef = createRef<HTMLDivElement>();

  /** Keeps the selected row in view. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.scrollSelectedIntoView, [this.props.selectedIdx]);
  }

  /** Scrolls the selected row (the list's n-th child) into view. */
  @bind
  protected scrollSelectedIntoView(): void {
    const row = this.listRef.current?.children[this.props.selectedIdx];
    row?.scrollIntoView({ block: "nearest" });
  }

  /**
   * Renders one prompt row.
   *
   * @param item - the prompt
   * @param i - its position
   */
  @bind
  protected renderItem(item: MyPromptItem, i: number) {
    return (
      <HistoryRow
        key={item.id}
        item={item}
        selected={i === this.props.selectedIdx}
        onRecall={this.props.onRecall}
        onJump={this.props.onJump}
      />
    );
  }

  /** Renders the prompt list, or the empty notice. */
  public render() {
    const items = this.useComputed(myPrompts);
    if (items.length === 0) {
      return (
        <div className={SHELL_CLASS}>
          <div className="px-4 py-3 text-sm text-text-subtle italic">No prompts in this session yet.</div>
        </div>
      );
    }
    return (
      <div className={SHELL_CLASS}>
        <div ref={this.listRef} className="max-h-72 overflow-y-auto">
          {items.map(this.renderItem)}
        </div>
        <div className="px-4 py-1.5 border-t border-surface/50 flex items-center gap-3 text-[11px] text-text-subtle">
          <span><kbd className="font-mono">↑↓</kbd> walk</span>
          <span><kbd className="font-mono">Enter</kbd> recall</span>
          <span><kbd className="font-mono">Esc</kbd> close</span>
        </div>
      </div>
    );
  }
}
