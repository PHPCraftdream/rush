import { createRef } from "react";
import type { ChangeEvent, KeyboardEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { Todo, TodoStatus } from "../types";
import { updateTodos } from "../store";

// ── Status symbol ─────────────────────────────────────────────────────────────

/** Shared inline style of the status glyphs. */
const SYMBOL_STYLE = { fontSize: "var(--chat-font-size)", lineHeight: 1 };

/**
 * The glyph of a todo's status.
 *
 * @param props - component props
 * @param props.status - the todo's status
 */
function StatusSymbol({ status }: { status: TodoStatus }) {
  if (status === "completed")
    return <span className="text-green shrink-0" style={SYMBOL_STYLE}>✓</span>;
  if (status === "in_progress")
    return <span className="text-accent shrink-0 animate-pulse" style={SYMBOL_STYLE}>◑</span>;
  return <span className="text-text-subtle shrink-0" style={SYMBOL_STYLE}>○</span>;
}

/**
 * Whether a todo is completed.
 *
 * @param t - the todo
 */
function isCompleted(t: Todo): boolean {
  return t.status === "completed";
}

/**
 * Whether a todo is not completed.
 *
 * @param t - the todo
 */
function isOpen(t: Todo): boolean {
  return t.status !== "completed";
}

/**
 * The status a click on the status button cycles to.
 *
 * @param s - the current status
 */
function nextStatus(s: TodoStatus): TodoStatus {
  if (s === "pending") return "in_progress";
  if (s === "in_progress") return "completed";
  return "pending";
}

// ── Single todo row ───────────────────────────────────────────────────────────

/** Props of one todo row. */
interface ITodoRowProps {
  todo: Todo;
  index: number;
  total: number;
  onChange: (index: number, todo: Todo) => void;
  onDelete: (index: number) => void;
  onMove: (index: number, dir: -1 | 1) => void;
}

/** Local state of one todo row. */
interface ITodoRowState {
  editing: boolean;
  draft: string;
}

/** One todo with its hover actions, status toggle and inline editor. */
class TodoRow extends AntiHookComponent<ITodoRowProps, ITodoRowState> {
  public state: ITodoRowState = { editing: false, draft: this.props.todo.content };

  private readonly inputRef = createRef<HTMLInputElement>();

  private editStartContent: string | null = null;

  /** Focuses the inline editor whenever editing starts. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("focusEditor", this.focusEditor, [this.state.editing]);
  }

  /** Focuses the editor input while editing. */
  @bind
  protected focusEditor(): void {
    if (this.state.editing) this.inputRef.current?.focus();
  }

  /** Opens the inline editor seeded with the current content. */
  @bind
  protected startEdit(): void {
    this.editStartContent = this.props.todo.content;
    this.setState({ draft: this.props.todo.content, editing: true });
  }

  /** Commits the edited content, or drops the edit if the todo changed underneath it. */
  @bind
  protected commitEdit(): void {
    const { todo, index, onChange } = this.props;
    // The row is keyed by array index, so a live session update can swap the todo under an open edit;
    // committing then would overwrite that new todo, so drop the edit instead.
    if (todo.content !== this.editStartContent) {
      this.cancelEdit();
      return;
    }
    const trimmed = this.state.draft.trim();
    if (trimmed && trimmed !== todo.content) onChange(index, { ...todo, content: trimmed });
    this.setState({ editing: false });
  }

  /** Closes the editor, discarding the draft. */
  @bind
  protected cancelEdit(): void {
    this.setState({ draft: this.props.todo.content, editing: false });
  }

  /**
   * Enter commits, Escape cancels.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent<HTMLInputElement>): void {
    if (e.key === "Enter") this.commitEdit();
    if (e.key === "Escape") this.cancelEdit();
  }

  /**
   * Stores the typed draft.
   *
   * @param e - the input change event
   */
  @bind
  protected onDraftChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ draft: e.target.value });
  }

  /** Moves the todo one position up. */
  @bind
  protected moveUp(): void {
    this.props.onMove(this.props.index, -1);
  }

  /** Moves the todo one position down. */
  @bind
  protected moveDown(): void {
    this.props.onMove(this.props.index, 1);
  }

  /** Deletes the todo. */
  @bind
  protected deleteSelf(): void {
    this.props.onDelete(this.props.index);
  }

  /** Cycles the todo's status. */
  @bind
  protected cycleStatus(): void {
    const { todo, index, onChange } = this.props;
    onChange(index, { ...todo, status: nextStatus(todo.status) });
  }

  /** Renders the row. */
  public render() {
    const { todo, index, total } = this.props;
    const { editing, draft } = this.state;

    return (
      <div
        data-test-id="todo-row"
        className="todo-row group"
      >
        {/* Actions — LEFT side, visible on hover */}
        <div className="flex items-center gap-0.5 shrink-0">
          {editing ? (
            <button
              data-test-id="todo-save"
              onClick={this.commitEdit}
              title="Save"
              className="w-5 h-5 flex items-center justify-center text-green hover:opacity-70 transition-opacity text-[13px] leading-none"
            >
              ✓
            </button>
          ) : (
            <div className="flex items-center gap-0 opacity-0 group-hover:opacity-100 transition-opacity">
              <button
                data-test-id="todo-edit"
                onClick={this.startEdit}
                title="Edit"
                className="w-5 h-5 flex items-center justify-center text-text-subtle hover:text-accent transition-colors text-[12px] leading-none"
              >
                ✏
              </button>
              <button
                data-test-id="todo-move-up"
                onClick={this.moveUp}
                disabled={index === 0}
                title="Move up"
                className="w-4 h-5 flex items-center justify-center text-text-subtle hover:text-text disabled:opacity-20 transition-colors text-[12px] leading-none"
              >
                ↑
              </button>
              <button
                data-test-id="todo-move-down"
                onClick={this.moveDown}
                disabled={index === total - 1}
                title="Move down"
                className="w-4 h-5 flex items-center justify-center text-text-subtle hover:text-text disabled:opacity-20 transition-colors text-[12px] leading-none"
              >
                ↓
              </button>
              <button
                data-test-id="todo-delete"
                onClick={this.deleteSelf}
                title="Delete"
                className="w-5 h-5 flex items-center justify-center text-text-subtle hover:text-red transition-colors text-[14px] leading-none"
              >
                ×
              </button>
            </div>
          )}
        </div>

        {/* Status toggle */}
        <button
          data-test-id="todo-status-btn"
          onClick={this.cycleStatus}
          title={`Status: ${todo.status}`}
          className="shrink-0 hover:opacity-70 transition-opacity"
        >
          <StatusSymbol status={todo.status} />
        </button>

        {/* Content / edit input */}
        <div className="flex-1 min-w-0">
          {editing ? (
            <input
              ref={this.inputRef}
              data-test-id="todo-edit-input"
              value={draft}
              onChange={this.onDraftChange}
              onKeyDown={this.onKey}
              className="field-inline" style={{ fontSize: "var(--chat-font-size)" }}
            />
          ) : (
            <span
              data-test-id="todo-content"
              className={`cursor-default select-none block truncate ${
                todo.status === "completed" ? "line-through text-text-subtle" : "text-text"
              }`} style={{ fontSize: "var(--chat-font-size)" }}
            >
              {todo.content}
            </span>
          )}
        </div>
      </div>
    );
  }
}

// ── New task input row ────────────────────────────────────────────────────────

/** Props of the new-task row. */
interface IAddTaskRowProps {
  onAdd: (content: string) => void;
  onCancel: () => void;
}

/** Local state of the new-task row. */
interface IAddTaskRowState {
  draft: string;
}

/** Inline input for a new task; Enter or blur commits, Escape cancels. */
class AddTaskRow extends AntiHookComponent<IAddTaskRowProps, IAddTaskRowState> {
  public state: IAddTaskRowState = { draft: "" };

  private readonly inputRef = createRef<HTMLInputElement>();

  /** Focuses the input once, on mount. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("focusInput", this.focusInput, []);
  }

  /** Focuses the input. */
  @bind
  protected focusInput(): void {
    this.inputRef.current?.focus();
  }

  /** Adds the trimmed draft, or cancels when it is blank. */
  @bind
  protected commit(): void {
    const trimmed = this.state.draft.trim();
    if (trimmed) this.props.onAdd(trimmed);
    else this.props.onCancel();
  }

  /**
   * Enter commits, Escape cancels.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent<HTMLInputElement>): void {
    if (e.key === "Enter") this.commit();
    if (e.key === "Escape") this.props.onCancel();
  }

  /**
   * Stores the typed draft.
   *
   * @param e - the input change event
   */
  @bind
  protected onDraftChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ draft: e.target.value });
  }

  /** Renders the row. */
  public render() {
    return (
      <div className="flex items-center gap-1.5 px-2 py-1.5">
        <span className="text-text-subtle shrink-0" style={SYMBOL_STYLE}>○</span>
        <input
          ref={this.inputRef}
          value={this.state.draft}
          onChange={this.onDraftChange}
          onKeyDown={this.onKey}
          onBlur={this.commit}
          placeholder="New task…"
          className="field-inline flex-1 min-w-0 placeholder:text-text-muted"
          style={{ fontSize: "var(--chat-font-size)" }}
        />
      </div>
    );
  }
}

// ── TodoList panel ────────────────────────────────────────────────────────────

/** Props of the todo panel. */
interface ITodoListProps {
  sessionID: string;
  todos: Todo[];
}

/** Local state of the todo panel. */
interface ITodoListState {
  collapsed: boolean;
  addingNew: boolean;
}

/** Tasks tab body: the session's todos with inline editing, reordering and clearing. */
export class TodoList extends AntiHookComponent<ITodoListProps, ITodoListState> {
  public state: ITodoListState = { collapsed: false, addingNew: false };

  /**
   * Sends the new todo list for the session.
   *
   * @param next - the full replacement list
   */
  protected update(next: Todo[]): void {
    updateTodos(this.props.sessionID, next);
  }

  /**
   * Replaces one todo.
   *
   * @param i - its index
   * @param t - the replacement
   */
  @bind
  protected changeTodo(i: number, t: Todo): void {
    const next = [...this.props.todos];
    next[i] = t;
    this.update(next);
  }

  /**
   * Removes one todo.
   *
   * @param i - its index
   */
  @bind
  protected deleteTodo(i: number): void {
    this.update(this.props.todos.filter((_, idx) => idx !== i));
  }

  /**
   * Swaps a todo with its neighbour.
   *
   * @param i - its index
   * @param dir - -1 for up, 1 for down
   */
  @bind
  protected moveTodo(i: number, dir: -1 | 1): void {
    const { todos } = this.props;
    const j = i + dir;
    if (j < 0 || j >= todos.length) return;
    const next = [...todos];
    [next[i], next[j]] = [next[j], next[i]];
    this.update(next);
  }

  /**
   * Appends a pending todo and closes the new-task row.
   *
   * @param content - the new todo's text
   */
  @bind
  protected addTask(content: string): void {
    this.update([...this.props.todos, { content, status: "pending", active_form: "" }]);
    this.setState({ addingNew: false });
  }

  /** Closes the new-task row. */
  @bind
  protected cancelAdding(): void {
    this.setState({ addingNew: false });
  }

  /** Drops every completed todo. */
  @bind
  protected clearCompleted(): void {
    this.update(this.props.todos.filter(isOpen));
  }

  /** Opens the new-task row, expanding the panel. */
  @bind
  protected startAdding(): void {
    this.setState({ collapsed: false, addingNew: true });
  }

  /** Collapses or expands the list. */
  @bind
  protected toggleCollapsed(): void {
    this.setState({ collapsed: !this.state.collapsed });
  }

  /**
   * Renders one todo row.
   *
   * @param t - the todo
   * @param i - its index
   */
  @bind
  protected renderRow(t: Todo, i: number) {
    return (
      <TodoRow
        key={i}
        todo={t}
        index={i}
        total={this.props.todos.length}
        onChange={this.changeTodo}
        onDelete={this.deleteTodo}
        onMove={this.moveTodo}
      />
    );
  }

  /** Renders the header and, unless collapsed, the list. */
  public render() {
    const { todos } = this.props;
    const { collapsed, addingNew } = this.state;
    const completed = todos.filter(isCompleted).length;
    const isEmpty = todos.length === 0;

    return (
      <div
        data-test-id="todo-list"
        // No own border-t/shrink-0 here (task #1059): LiveWorkPanel's tab bar
        // above already supplies the panel's top divider; this is now always
        // mounted as one tab's body, not the panel's own outer shell.
        className="bg-base-subtle/40"
      >
        {/* Header */}
        <div className="flex items-center">
          <button
            data-test-id="todo-list-toggle"
            onClick={this.toggleCollapsed}
            className="shrink-0 flex items-center gap-2 px-4 py-2 text-left hover:bg-base-overlay/50 transition-colors"
          >
            <span
              className={`text-text-subtle text-[11px] transition-transform inline-block ${
                collapsed ? "" : "rotate-90"
              }`}
            >
              ▶
            </span>
            <span className="font-semibold text-text-subtle uppercase tracking-wider" style={{ fontSize: "var(--chat-font-size)" }}>
              Tasks
            </span>
            {!isEmpty && (
              <span className="text-text-muted ml-1" style={{ fontSize: "var(--chat-font-size)" }}>
                {completed}/{todos.length}
              </span>
            )}
          </button>

          {/* StatusBar now lives in LiveWorkTabBar (task #1059) -- it's
              rendered once for the whole tabbed panel, not per-tab. This
              spacer just keeps the action buttons pinned to the right. */}
          <div className="flex-1" />

          {/* Clear completed / Add task buttons */}
          {completed > 0 && (
            <button
              data-test-id="todo-clear-completed"
              onClick={this.clearCompleted}
              title="Clear completed tasks"
              className="px-2 py-1.5 text-text-subtle hover:text-red transition-colors text-[11px] leading-none"
            >
              ✕ Done
            </button>
          )}
          <button
            data-test-id="todo-add-btn"
            onClick={this.startAdding}
            title="Add task"
            className="px-3 py-2 text-text-subtle hover:text-text transition-colors text-[18px] leading-none"
          >
            +
          </button>
        </div>

        {/* List */}
        {!collapsed && (
          <div className="px-2 pb-2">
            {isEmpty && !addingNew && (
              <p className="text-text-muted px-2 py-1" style={{ fontSize: "var(--chat-font-size)" }}>No tasks yet.</p>
            )}
            {todos.map(this.renderRow)}
            {addingNew && <AddTaskRow onAdd={this.addTask} onCancel={this.cancelAdding} />}
          </div>
        )}
      </div>
    );
  }
}
