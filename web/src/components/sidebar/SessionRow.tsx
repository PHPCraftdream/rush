import { createRef } from "react";
import type { ChangeEvent, KeyboardEvent, MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { Pencil, X, Check } from "lucide-react";
import type { Session } from "../../types";
import { busyCarburetor } from "../../state/busy";
import { sessionsCarburetor } from "../../state/sessions";
import { formatTokens } from "./formatTokens";

/** Props of one session row. */
interface ISessionRowProps {
  session: TReadonly<Session>;
  isEditing: boolean;
  /** The rename draft; only meaningful while this row is being edited. */
  editTitle: string;
  onSelect: (id: string) => void;
  onStartEdit: (id: string, title: string) => void;
  onDelete: (id: string, title: string) => void;
  onEditTitleChange: (title: string) => void;
  onEditKeyDown: (e: KeyboardEvent) => void;
  onSaveRename: () => void;
  onCancelEdit: () => void;
}

/** One session of the sidebar list, with inline rename and hover actions. */
export class SessionRow extends AntiHookComponent<ISessionRowProps> {
  private readonly inputRef = createRef<HTMLInputElement>();

  /** Focuses and selects the rename input whenever editing starts. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("focusEdit", this.focusEdit, [this.props.isEditing]);
  }

  /** Focuses the rename input while editing. */
  @bind
  protected focusEdit(): void {
    if (this.props.isEditing && this.inputRef.current) {
      this.inputRef.current.focus();
      this.inputRef.current.select();
    }
  }

  /** Makes this session the active one. */
  @bind
  protected select(): void {
    this.props.onSelect(this.props.session.ID);
  }

  /**
   * Starts renaming this session.
   *
   * @param e - the double-click or click event
   */
  @bind
  protected startEdit(e: MouseEvent): void {
    e.stopPropagation();
    this.props.onStartEdit(this.props.session.ID, this.props.session.Title);
  }

  /**
   * Asks to delete this session.
   *
   * @param e - the click event
   */
  @bind
  protected requestDelete(e: MouseEvent): void {
    e.stopPropagation();
    this.props.onDelete(this.props.session.ID, this.props.session.Title);
  }

  /**
   * Keeps clicks inside the editor from selecting the row.
   *
   * @param e - the click event
   */
  @bind
  protected stopPropagation(e: MouseEvent): void {
    e.stopPropagation();
  }

  /**
   * Stores the typed title.
   *
   * @param e - the input change event
   */
  @bind
  protected onTitleInput(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onEditTitleChange(e.target.value);
  }

  /**
   * Saves the rename.
   *
   * @param e - the click event
   */
  @bind
  protected saveClick(e: MouseEvent): void {
    e.stopPropagation();
    this.props.onSaveRename();
  }

  /**
   * Cancels the rename.
   *
   * @param e - the click event
   */
  @bind
  protected cancelClick(e: MouseEvent): void {
    e.stopPropagation();
    this.props.onCancelEdit();
  }

  /**
   * Renders the title area: the rename editor or the title.
   *
   * @param isActive - whether this is the active session
   */
  protected renderHeading(isActive: boolean) {
    const { session: s, isEditing, editTitle, onEditKeyDown } = this.props;
    if (isEditing) {
      return (
        <div className="flex-1 flex flex-col gap-1.5 min-w-0" onClick={this.stopPropagation}>
          <input
            ref={this.inputRef}
            value={editTitle}
            onChange={this.onTitleInput}
            onKeyDown={onEditKeyDown}
            data-test-id="session-edit-input"
            className="font-medium w-full bg-canvas border border-accent rounded-lg px-2 py-1 outline-none shadow-sm"
            style={{ fontSize: "var(--chat-font-size)" }}
          />
          <div className="flex gap-1.5">
            <button
              onClick={this.saveClick}
              title="Save (Enter)"
              data-test-id="session-edit-save"
              className="flex items-center gap-1 px-2 py-1 rounded-lg btn-primary text-xs font-semibold"
            >
              <Check size={12} /> Save
            </button>
            <button
              onClick={this.cancelClick}
              title="Cancel (Esc)"
              data-test-id="session-edit-cancel"
              className="flex items-center gap-1 px-2 py-1 rounded-lg bg-base-overlay text-text-subtle hover:text-text border border-surface text-xs"
            >
              <X size={12} /> Cancel
            </button>
          </div>
        </div>
      );
    }
    return (
      <div
        data-test-id={`session-title-${s.ID}`}
        className={`font-semibold ${isActive ? "text-accent" : "text-text"}`}
        style={{ fontSize: "var(--chat-font-size)" }}
      >
        {s.Title || "Untitled session"}
      </div>
    );
  }

  /** Renders the row. */
  public render() {
    const { session: s, isEditing } = this.props;
    const { activeSessionID } = this.useCarburetor(sessionsCarburetor);
    const { busySessions } = this.useCarburetor(busyCarburetor);
    const isActive = s.ID === activeSessionID;
    const isBusy = busySessions.includes(s.ID);
    const totalTokens = s.PromptTokens + s.CompletionTokens;

    return (
      <div
        onClick={this.select}
        onDoubleClick={this.startEdit}
        data-test-id={`session-${s.ID}`}
        data-session-id={s.ID}
        className={`group relative px-4 py-4 rounded-xl cursor-pointer transition-all mb-1 ${
          isActive
            ? "bg-canvas shadow-sm border border-accent/20"
            : "hover:bg-canvas/50 border border-transparent"
        }`}
      >
        <div className={`flex items-start gap-3 ${!isEditing ? "pr-12" : ""}`}>
          {isBusy && !isEditing && (
            <span
              data-test-id={`session-busy-${s.ID}`}
              className="w-2 h-2 rounded-full bg-accent shrink-0 animate-pulse mt-2"
            />
          )}
          {this.renderHeading(isActive)}
        </div>
        {!isEditing && (
          <div
            className="flex items-center gap-2.5 mt-1 text-text-subtle pl-0 font-medium"
            style={{ fontSize: "var(--chat-font-size)" }}
          >
            <span data-test-id={`session-msg-count-${s.ID}`}>
              {s.MessageCount} msg{s.MessageCount !== 1 ? "s" : ""}
            </span>
            {totalTokens > 0 && (
              <>
                <span>·</span>
                <span data-test-id={`session-tokens-${s.ID}`}>{formatTokens(totalTokens)} tok</span>
              </>
            )}
          </div>
        )}

        {!isEditing && (
          <div className="session-hover-btns">
            <button
              onClick={this.startEdit}
              title="Rename session"
              data-test-id={`session-rename-${s.ID}`}
              className="sidebar-icon-btn"
            >
              <Pencil size={14} />
            </button>
            <button
              onClick={this.requestDelete}
              title="Delete session"
              data-test-id={`session-delete-${s.ID}`}
              className="sidebar-icon-btn-danger"
            >
              <X size={16} />
            </button>
          </div>
        )}
      </div>
    );
  }
}
