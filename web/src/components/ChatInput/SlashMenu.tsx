import { createRef } from "react";
import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { SkillInfo } from "../../types";
import { sourceBadgeClass } from "./sourceBadgeClass";

/** Props of one slash-menu row. */
interface ISlashMenuItemProps {
  skill: SkillInfo;
  selected: boolean;
  onSelect: (skill: SkillInfo, sendNow: boolean) => void;
}

/** One skill row of the slash menu. */
class SlashMenuItem extends AntiHookComponent<ISlashMenuItemProps> {
  /**
   * Fills the skill in without stealing focus from the textarea.
   *
   * @param e - the mousedown event
   */
  @bind
  protected onMouseDown(e: MouseEvent<HTMLButtonElement>): void {
    e.preventDefault(); // keep focus on textarea
    this.props.onSelect(this.props.skill, false);
  }

  /** Renders the row. */
  public render() {
    const { skill, selected } = this.props;
    const tone = selected
      ? "bg-accent/20 border-l-2 border-l-accent pl-[14px]"
      : "hover:bg-base-subtle border-l-2 border-l-transparent pl-[14px]";
    const base = "w-full text-left px-4 py-2.5 flex items-center gap-3 transition-colors border-b border-surface/50 last:border-0";
    const badge = "text-[10px] px-1.5 py-0.5 rounded font-mono font-medium";
    return (
      <button onMouseDown={this.onMouseDown} className={`${base} ${tone}`}>
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2">
            <span className={`font-semibold text-sm ${selected ? "text-accent" : "text-text"}`}>/{skill.name}</span>
            {skill.source && <span className={`${badge} ${sourceBadgeClass(skill.source)}`}>{skill.source}</span>}
          </div>
          <p className="text-text-muted text-xs truncate mt-0.5 leading-relaxed">{skill.description}</p>
        </div>
      </button>
    );
  }
}

/** Props of the slash menu. */
interface ISlashMenuProps {
  items: readonly SkillInfo[];
  selectedIdx: number;
  onSelect: (skill: SkillInfo, sendNow: boolean) => void;
}

/** Skill dropdown shown above the composer while the draft is a bare "/command". */
export class SlashMenu extends AntiHookComponent<ISlashMenuProps> {
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
   * Renders one skill row.
   *
   * @param skill - the skill
   * @param i - its position
   */
  @bind
  protected renderItem(skill: SkillInfo, i: number) {
    return (
      <SlashMenuItem
        key={`${skill.source}-${skill.name}`}
        skill={skill}
        selected={i === this.props.selectedIdx}
        onSelect={this.props.onSelect}
      />
    );
  }

  /** Renders the list and its key hints. */
  public render() {
    if (this.props.items.length === 0) return null;
    return (
      <div
        className={
          "absolute bottom-full left-0 right-0 mb-2 bg-base-overlay border border-surface rounded-2xl " +
          "shadow-2xl overflow-hidden z-50"
        }
      >
        <div ref={this.listRef} className="max-h-72 overflow-y-auto">
          {this.props.items.map(this.renderItem)}
        </div>
        <div className="px-4 py-1.5 border-t border-surface/50 flex items-center gap-3 text-[11px] text-text-subtle">
          <span><kbd className="font-mono">↑↓</kbd> navigate</span>
          <span><kbd className="font-mono">Tab</kbd> fill</span>
          <span><kbd className="font-mono">Enter</kbd> send</span>
          <span><kbd className="font-mono">Esc</kbd> close</span>
        </div>
      </div>
    );
  }
}
