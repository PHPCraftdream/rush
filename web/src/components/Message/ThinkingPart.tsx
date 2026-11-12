// Collapsible reasoning card with edit/delete affordances.

import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { BrainCircuit, Pencil, Trash2, ChevronDown, ChevronUp } from "lucide-react";
import { updateMessageThinking, deleteMessagePart } from "../../store";
import { ConfirmDialog } from "../ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { EditForm } from "./EditForm";
import { EffortBadge } from "./EffortBadge";
import { watchCollapseAllSignal } from "./watchCollapseAllSignal";

/** Props of the thinking card. */
interface IThinkingPartProps {
  thinking: string;
  messageID: string;
  partIndex: number;
  done: boolean;
  model?: string;
  effort?: string;
}

/** Local state of the thinking card: owns its own edit/delete state. */
interface IThinkingPartState {
  editing: boolean;
  confirmDelete: boolean;
  // Controlled open-state (not <details>) so we can render a sticky
  // collapse strip at the bottom — long reasoning blocks are tedious to
  // close when the toggle is only at the top.
  open: boolean;
}

/** Collapsible reasoning card; streaming reasoning renders fully with no toggle. */
export class ThinkingPart extends AntiHookComponent<IThinkingPartProps, IThinkingPartState> {
  public state: IThinkingPartState = { editing: false, confirmDelete: false, open: false };

  /** Subscribes to the global "Collapse all" signal. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.watchCollapseAll, []);
  }

  /** Starts watching the collapse-all signal; returns its disposer. */
  @bind
  protected watchCollapseAll(): () => void {
    return watchCollapseAllSignal(this.collapse);
  }

  // Collapsing the card (toggle, sticky bottom strip, or "Collapse all")
  // also exits edit mode — a stashed edit form that silently reappears on
  // the next expand is the same ambush shape as the delete dialog.
  /** Folds the card and leaves edit mode. */
  @bind
  protected collapse(): void {
    this.setState({ open: false, editing: false });
  }

  /** Leaves edit mode without saving. */
  @bind
  protected closeEdit(): void {
    this.setState({ editing: false });
  }

  /**
   * Opens the delete confirmation without toggling the card.
   *
   * @param e - the click event
   */
  @bind
  protected openDel(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    this.setState({ confirmDelete: true });
  }

  /** Closes the delete confirmation. */
  @bind
  protected cancelDelete(): void {
    this.setState({ confirmDelete: false });
  }

  // Editing a collapsed card implicitly means the operator now wants to see
  // and edit its content, so force the card open (the EditForm renders in
  // the card body).
  /**
   * Enters edit mode and forces the card open.
   *
   * @param e - the click event
   */
  @bind
  protected openEditEv(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    this.setState({ editing: true, open: true });
  }

  /** Expands a folded card, or folds an open one. */
  @bind
  protected toggleOpen(): void {
    if (this.state.open) this.collapse();
    else this.setState({ open: true });
  }

  /**
   * Keeps clicks on the action cluster from toggling the card.
   *
   * @param e - the click event
   */
  @bind
  protected stopPropagation(e: MouseEvent): void {
    e.stopPropagation();
  }

  /**
   * Persists a changed thought and leaves edit mode.
   *
   * @param text - the edited text
   */
  @bind
  protected handleSave(text: string): void {
    const { thinking, messageID, partIndex } = this.props;
    if (text && text !== thinking) updateMessageThinking(messageID, text, partIndex);
    this.setState({ editing: false });
  }

  /** Deletes the part and closes the confirmation. */
  @bind
  protected handleConfirmDelete(): void {
    const { messageID, partIndex } = this.props;
    deleteMessagePart(messageID, partIndex);
    this.setState({ confirmDelete: false });
  }

  /** Renders the streaming card (no toggle) or the collapsible finished card. */
  public render() {
    const { thinking, done, model, effort } = this.props;
    const { editing, confirmDelete, open } = this.state;

    // While the model is still thinking we always render fully (no toggle);
    // streaming reasoning hidden behind a click is useless.
    if (!done) {
      return (
        <div data-test-id="thinking-card" className="thinking-card">
          <div className="thinking-card-header">
            <BrainCircuit size={15} className="text-accent/70 shrink-0 animate-pulse" />
            <span data-test-id="thinking-label">Thinking…</span>
            {model && <span className="text-xs text-text-subtle font-mono">{model}</span>}
            <EffortBadge effort={effort} />
          </div>
          {thinking && (
            <pre
              data-test-id="thinking-content"
              className="px-4 pb-3 font-mono whitespace-pre-wrap text-text-subtle leading-relaxed max-h-40 overflow-y-auto border-t border-surface/50"
              style={{ fontSize: "var(--chat-font-size)" }}
            >
              {thinking}
            </pre>
          )}
        </div>
      );
    }

    return (
      <div data-test-id="thinking-card" className="thinking-card-done group">
        <button
          type="button"
          onClick={this.toggleOpen}
          data-test-id="thinking-toggle"
          className="thinking-toggle w-full text-left"
          aria-expanded={open}
        >
          <span className="text-accent/70"><BrainCircuit size={18} /></span>
          <span data-test-id="thinking-label">Thoughts</span>
          {model && <span className="text-xs text-text-subtle font-mono">{model}</span>}
          <EffortBadge effort={effort} />
          <div className="ml-auto flex items-center gap-0.5 hover-reveal" onClick={this.stopPropagation}>
            <CopyButton text={thinking} className="px-1.5 py-1 text-xs" />
            <button onClick={this.openEditEv} title="Edit thinking" className="btn-icon-sm">
              <Pencil size={13} />
            </button>
            <button onClick={this.openDel} title="Delete thinking" className="btn-icon-sm-danger">
              <Trash2 size={13} />
            </button>
          </div>
          <span className="text-text-subtle ml-1 shrink-0">
            {open ? <ChevronUp size={16} /> : <ChevronDown size={16} />}
          </span>
        </button>
        {/* The confirm dialog is a fixed overlay and must render regardless of
            the card's open state — the Delete button lives in the collapsed
            header, so gating on `open` swallowed the click and ambushed the
            operator with the destructive dialog on a later, unrelated expand
            (ActionRow.tsx renders its dialog the same unguarded way). */}
        {confirmDelete && (
          <ConfirmDialog
            title="Delete thinking"
            message="The model's reasoning will be removed from this message. This cannot be undone."
            confirmLabel="Delete"
            onConfirm={this.handleConfirmDelete}
            onCancel={this.cancelDelete}
          />
        )}
        {/* The EditForm is likewise ungated: `editing` is only ever true while
            open (openEditEv forces open, collapse clears editing), and keeping
            the form outside the open guard means a future regression can never
            re-introduce the swallowed click. */}
        {editing ? (
          <div className="p-4 bg-base-overlay border-t border-surface">
            <EditForm
              initialValue={thinking}
              rows={6}
              className="w-full bg-base-subtle border border-accent/40 text-text-muted rounded-lg px-4 py-3 text-[14px] font-mono leading-relaxed resize-none outline-none focus:border-accent"
              onSave={this.handleSave}
              onCancel={this.closeEdit}
            />
          </div>
        ) : open ? (
          <>
            <pre
              data-test-id="thinking-content"
              className="p-5 bg-base-overlay font-mono whitespace-pre-wrap overflow-x-auto text-text-muted border-t border-surface leading-relaxed"
              style={{ fontSize: "var(--chat-font-size)" }}
            >
              {thinking}
            </pre>
            {/* Sticky bottom collapse strip: stays visible as the user scrolls
                past the reasoning so closing it is one click away — no need to
                scroll back up to the header toggle. */}
            <button
              type="button"
              onClick={this.toggleOpen}
              data-test-id="thinking-collapse-bottom"
              className="thinking-collapse-bottom"
              title="Collapse thinking"
            >
              <ChevronUp size={14} />
              <span>Collapse thinking</span>
            </button>
          </>
        ) : null}
      </div>
    );
  }
}
