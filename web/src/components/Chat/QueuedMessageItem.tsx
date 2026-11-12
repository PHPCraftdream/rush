import { createRef } from "react";
import type { ChangeEvent, KeyboardEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { Pencil, PlusCircle, Send, Trash2, Zap } from "lucide-react";
import { interruptAndSendQueuedMessage, sendQueuedMessageDirect, sendQueuedMessageNow } from "../../store";
import type { QueuedMessage } from "../../store";
import { queueCarburetor } from "../../state/queue";

/** Props of one queued-message row. */
interface IQueuedMessageItemProps {
  item: TReadonly<QueuedMessage>;
  sessionID: string;
  position: number;
  total: number;
  /**
   * Send-now / interrupt-and-send only make sense while a turn is running
   * (task #1057), mirroring ChatInput's Inject/Interrupt. While idle a
   * plain Send (send_message) is offered instead, so a message restored
   * after the turn already ended doesn't get stranded until some future
   * turn happens to flush it.
   */
  sessionBusy: boolean;
}

/** Local edit state of a queued-message row. */
interface IQueuedMessageItemState {
  editing: boolean;
  draft: string;
}

/**
 * Grows a textarea to fit its content.
 *
 * @param el - the textarea
 */
const fitHeight = (el: HTMLTextAreaElement): void => {
  el.style.height = "auto";
  el.style.height = el.scrollHeight + "px";
};

/** One client-queued message with inline edit, send-now and remove actions. */
export class QueuedMessageItem extends AntiHookComponent<IQueuedMessageItemProps, IQueuedMessageItemState> {
  public state: IQueuedMessageItemState = { editing: false, draft: "" };

  private readonly taRef = createRef<HTMLTextAreaElement>();

  /** Focuses and sizes the editor each time editing starts. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.focusEditor, [this.state.editing]);
  }

  /** Puts the caret at the end of the freshly opened editor and fits its height. */
  @bind
  protected focusEditor(): void {
    const ta = this.taRef.current;
    if (this.state.editing && ta) {
      ta.focus();
      ta.selectionStart = ta.value.length;
      fitHeight(ta);
    }
  }

  /** Opens the editor seeded with the queued text. */
  @bind
  protected startEdit(): void {
    this.setState({ draft: this.props.item.content, editing: true });
  }

  /** Closes the editor without saving. */
  @bind
  protected cancelEdit(): void {
    this.setState({ editing: false });
  }

  /** Saves a non-empty draft into the queue and closes the editor. */
  @bind
  protected save(): void {
    const trimmed = this.state.draft.trim();
    if (trimmed) queueCarburetor.updateContent(this.props.sessionID, this.props.item.id, trimmed);
    this.setState({ editing: false });
  }

  /**
   * Escape cancels, Ctrl/Cmd+Enter saves.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent<HTMLTextAreaElement>): void {
    if (e.key === "Escape") this.setState({ editing: false });
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) this.save();
  }

  /**
   * Tracks the draft and grows the textarea.
   *
   * @param e - the change event
   */
  @bind
  protected onInput(e: ChangeEvent<HTMLTextAreaElement>): void {
    this.setState({ draft: e.target.value });
    fitHeight(e.target);
  }

  /** Removes the message from the queue. */
  @bind
  protected handleRemove(): void {
    queueCarburetor.remove(this.props.sessionID, this.props.item.id);
  }

  /**
   * Sends the message into the running turn without interrupting it.
   *
   * Both send paths are fire-and-forget: the item vanishes from the queue the
   * instant one of these is clicked (takeQueuedMessage runs synchronously inside
   * them), so there is nothing local to await here. A failed send re-adds the
   * item with `error` set, which re-mounts this component with the error visible.
   */
  @bind
  protected handleSendNow(): void {
    void sendQueuedMessageNow(this.props.sessionID, this.props.item.id);
  }

  /** Interrupts the running turn and sends this message immediately. */
  @bind
  protected handleInterruptSend(): void {
    void interruptAndSendQueuedMessage(this.props.sessionID, this.props.item.id);
  }

  /** Idle-only path (task #1057): fire-and-forget, see sendQueuedMessageDirect. */
  @bind
  protected handleSendDirect(): void {
    sendQueuedMessageDirect(this.props.sessionID, this.props.item.id);
  }

  /** Renders the inline editor. */
  protected renderEditor() {
    return (
      <div className="flex flex-col gap-2">
        <textarea
          ref={this.taRef}
          value={this.state.draft}
          onChange={this.onInput}
          onKeyDown={this.onKey}
          rows={1}
          className={
            "bg-surface/60 border border-accent/40 text-text rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] " +
            "leading-relaxed resize-none outline-none focus:border-accent w-full min-w-[280px]"
          }
          style={{ overflow: "hidden" }}
        />
        <div className="flex gap-2 justify-end">
          <button onClick={this.cancelEdit} className="btn-ghost-sm">Cancel</button>
          <button onClick={this.save} className="px-3 py-1 text-xs btn-primary">Save</button>
        </div>
      </div>
    );
  }

  /** Renders the send actions: inject/interrupt while busy, a plain Send while idle. */
  protected renderSendActions() {
    if (this.props.sessionBusy) {
      return (
        <>
          <button
            onClick={this.handleSendNow}
            title="Send now — merge into the running turn without interrupting it"
            data-test-id="queued-message-send-now"
            className="p-1.5 text-text-subtle hover:text-green transition-colors rounded"
          >
            <PlusCircle size={13} />
          </button>
          <button
            onClick={this.handleInterruptSend}
            title="Interrupt current turn and send this immediately"
            data-test-id="queued-message-interrupt-send"
            className="p-1.5 text-text-subtle hover:text-yellow transition-colors rounded"
          >
            <Zap size={13} />
          </button>
        </>
      );
    }
    // Idle: inject/interrupt don't apply (task #1057) -- a
    // plain Send starts a fresh turn via the normal send_message
    // path, same as the composer's own Send button when idle.
    return (
      <button
        onClick={this.handleSendDirect}
        title="Send now"
        data-test-id="queued-message-send"
        className="p-1.5 text-text-subtle hover:text-accent transition-colors rounded"
      >
        <Send size={13} />
      </button>
    );
  }

  /** Renders the read-only bubble, its error and the action strip. */
  protected renderBubble() {
    const { item, position, total } = this.props;
    const bubbleTone = item.error ? "border-red/50" : "border-surface";
    return (
      <>
        <div className="relative">
          <span
            className={
              "absolute -top-2.5 right-3 text-[10px] font-semibold text-text-subtle bg-canvas border " +
              "border-surface rounded-full px-1.5 py-0.5 leading-none"
            }
          >
            #{position}/{total}
          </span>
          <div
            className={
              "bg-surface/60 border text-text-subtle rounded-2xl rounded-tr-sm px-5 py-3.5 text-[16px] " +
              `leading-relaxed whitespace-pre-wrap ${bubbleTone}`
            }
          >
            {item.content}
          </div>
        </div>
        {item.error && (
          <div data-test-id="queued-message-error" className="text-[11px] text-red text-right mt-1">
            {item.error}
          </div>
        )}
        <div
          className="flex items-center justify-end gap-1 mt-1.5 opacity-0 group-hover/qi:opacity-100 transition-opacity"
        >
          {this.renderSendActions()}
          <button onClick={this.startEdit} title="Edit queued message" className="btn-icon">
            <Pencil size={13} />
          </button>
          <button onClick={this.handleRemove} title="Remove from queue" className="btn-icon-danger">
            <Trash2 size={13} />
          </button>
        </div>
      </>
    );
  }

  /** Renders the row. */
  public render() {
    return (
      <div className="group/qi flex justify-end px-8 py-2">
        <div className="max-w-[80%]">{this.state.editing ? this.renderEditor() : this.renderBubble()}</div>
      </div>
    );
  }
}
