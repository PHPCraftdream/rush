// One accordion row inside a tool activity group (a tool call/result pair
// or a thinking part).

import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { BrainCircuit, Pencil, Trash2, ChevronDown, ChevronUp } from "lucide-react";
import type { ContentPart } from "../../types";
import type { AsyncJobCompletion } from "../../asyncJobCompletion";
import { updateMessagePart, deleteMessagePart } from "../../store";
import { formatActionArgs } from "../../toolFormat";
import { asyncJobCompletions } from "../../state/asyncJobCompletions";
import { ConfirmDialog } from "../ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { EditForm } from "./EditForm";
import { EffortBadge } from "./EffortBadge";
import { TimeBadge } from "./TimeBadge";
import { ToolCallBlock } from "./ToolCallBlock";
import { ToolResultBlock } from "./ToolResultBlock";
import { watchExpandToolCallSignal } from "./watchExpandToolCallSignal";
import { isPendingJob } from "../../asyncJobMetadata";

/** A tool call row: the call and/or its paired result. */
export interface ToolActionItem {
  kind: "tool";
  callPart?: ContentPart & { type: "tool_call"; ID: string; Name: string; Input: string; Finished: boolean };
  resultPart?: ContentPart & {
    type: "tool_result"; ToolCallID: string; Name: string; Content: string; IsError: boolean; Metadata?: string;
  };
  idx: number;
  key: string;
  createdAt?: number;
  repeatCount?: number;
  model?: string;
  effort?: string;
}

/** A thinking row. */
export interface ThinkingActionItem {
  kind: "thinking";
  text: string;
  idx: number;
  key: string;
  createdAt?: number;
  messageID?: string;
  partIndex: number;
  model?: string;
  effort?: string;
}

/** One accordion row. */
export type ActionItem = ToolActionItem | ThinkingActionItem;

/** Props of an accordion row. */
interface IActionRowProps {
  item: ActionItem;
  isCurrent: boolean;
  suppressAutoCurrent: boolean;
  model?: string;
  effort?: string;
}

/** Local state of an accordion row. */
interface IActionRowState {
  // override:
  //   undefined → follow auto-rule (open iff isCurrent, AND auto isn't suppressed)
  //   true / false → user pinned, ignore auto-rule from now on
  override: boolean | undefined;
  // Used only by the thinking branch.
  editingThinking: boolean;
  confirmDeleteThinking: boolean;
}

/**
 * The tool call id a row answers to; undefined for thinking rows -- they
 * aren't addressable from the live-work panel.
 *
 * @param item - the row's item
 */
const toolCallIDOf = (item: ActionItem): string | undefined =>
  item.kind === "tool" ? (item.callPart?.ID ?? item.resultPart?.ToolCallID) : undefined;

/** One accordion row: a tool call/result pair, or a thinking part. */
export class ActionRow extends AntiHookComponent<IActionRowProps, IActionRowState> {
  public state: IActionRowState = { override: undefined, editingThinking: false, confirmDeleteThinking: false };

  /** Subscribes to jump-to-tool-call requests (task #1059) for this row's tool call. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.watchExpand, [toolCallIDOf(this.props.item)]);
  }

  /** Starts watching jump requests; returns its disposer. Forces the row open when LiveWorkPanel targets this call. */
  @bind
  protected watchExpand(): () => void {
    return watchExpandToolCallSignal(toolCallIDOf(this.props.item), this.forceOpen);
  }

  /** Pins the row open. */
  @bind
  protected forceOpen(): void {
    this.setState({ override: true });
  }

  /** Whether the row is currently open: the user's pin, else the auto-rule. */
  private isOpen(): boolean {
    const { isCurrent, suppressAutoCurrent } = this.props;
    return this.state.override ?? (suppressAutoCurrent ? false : isCurrent);
  }

  // Collapsing the row via its own toggle also exits edit mode — a stashed
  // edit form that silently reappears on the next expand is the same ambush
  // shape ThinkingPart guards against (55d32c4d). Tool rows are unaffected.
  /** Pins the row closed and leaves thinking edit mode. */
  @bind
  protected collapse(): void {
    this.setState({ override: false, editingThinking: false });
  }

  /** Opens a closed row, or collapses an open one. */
  @bind
  protected toggle(): void {
    if (this.isOpen()) this.collapse();
    else this.setState({ override: true });
  }

  /**
   * Keeps clicks on the action cluster from toggling the row.
   *
   * @param e - the click event
   */
  @bind
  protected stopPropagation(e: MouseEvent): void {
    e.stopPropagation();
  }

  // The row body (including EditForm) is gated on `open`; editing a collapsed
  // row implies wanting to see it, so force it open (mirrors ThinkingPart.tsx's
  // openEditEv) — otherwise the click sets state but renders nothing (the
  // sibling bug #676 fixed).
  /**
   * Enters thinking edit mode and forces the row open.
   *
   * @param e - the click event
   */
  @bind
  protected startEditThinking(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    this.setState({ editingThinking: true, override: true });
  }

  /**
   * Opens the thinking delete confirmation.
   *
   * @param e - the click event
   */
  @bind
  protected askDeleteThinking(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    this.setState({ confirmDeleteThinking: true });
  }

  /**
   * Persists the edited thinking text and leaves edit mode.
   *
   * @param text - the edited text
   */
  @bind
  protected saveThinking(text: string): void {
    const { item } = this.props;
    if (item.kind !== "thinking") return;
    updateMessagePart(item.messageID ?? "", item.partIndex ?? -1, text);
    this.setState({ editingThinking: false });
  }

  /** Leaves thinking edit mode without saving. */
  @bind
  protected cancelEditThinking(): void {
    this.setState({ editingThinking: false });
  }

  /** Deletes the thinking part and closes the confirmation. */
  @bind
  protected confirmThinkingDelete(): void {
    const { item } = this.props;
    if (item.kind !== "thinking") return;
    deleteMessagePart(item.messageID ?? "", item.partIndex ?? -1);
    this.setState({ confirmDeleteThinking: false });
  }

  /** Closes the thinking delete confirmation. */
  @bind
  protected cancelDeleteThinking(): void {
    this.setState({ confirmDeleteThinking: false });
  }

  /**
   * Renders a thinking row.
   *
   * Thinking rows live alongside tool rows in the
   * accordion. Same open/close + auto-current rules; collapsed header shows a
   * one-line preview of the model's reasoning so the operator can scan the
   * chain without expanding every row.
   *
   * @param item - the thinking item
   */
  private renderThinking(item: ThinkingActionItem) {
    const { model, effort } = this.props;
    const { editingThinking, confirmDeleteThinking } = this.state;
    const open = this.isOpen();
    const preview = item.text.replace(/\s+/g, " ").trim();
    const messageID = item.messageID ?? "";
    const partIndex = item.partIndex ?? -1;
    return (
      <div data-test-id="action-row" className="action-row group">
        <button
          type="button"
          onClick={this.toggle}
          aria-expanded={open}
          data-test-id="action-row-toggle"
          className="action-row-head"
          title={preview || "thinking"}
        >
          <span className="text-accent/70 shrink-0"><BrainCircuit size={13} /></span>
          <span className="text-accent/80 font-semibold text-sm shrink-0">thinking</span>
          {model && <span className="text-xs text-text-subtle font-mono shrink-0">{model}</span>}
          <EffortBadge effort={effort} extraClass="shrink-0" />
          <span className="text-text font-mono text-sm truncate flex-1 min-w-0">
            {preview || "—"}
          </span>
          <TimeBadge epochSec={item.createdAt} />
          {messageID && partIndex >= 0 && (
            <div className="flex items-center gap-0.5 hover-reveal shrink-0" onClick={this.stopPropagation}>
              <CopyButton text={item.text} className="px-1.5 py-1 text-xs" />
              <button onClick={this.startEditThinking} title="Edit thinking" className="btn-icon-sm">
                <Pencil size={13} />
              </button>
              <button onClick={this.askDeleteThinking} title="Delete thinking" className="btn-icon-sm-danger">
                <Trash2 size={13} />
              </button>
            </div>
          )}
          <span className="text-text-subtle shrink-0">
            {open ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
          </span>
        </button>
        {open && (
          <div className="action-row-body">
            {editingThinking ? (
              <div className="p-4 bg-base-overlay border-t border-surface">
                <EditForm
                  initialValue={item.text}
                  rows={6}
                  className="w-full bg-base-subtle border border-accent/40 text-text-muted rounded-lg px-4 py-3 text-[14px] font-mono leading-relaxed resize-none outline-none focus:border-accent"
                  onSave={this.saveThinking}
                  onCancel={this.cancelEditThinking}
                />
              </div>
            ) : (
              <pre className="tool-output whitespace-pre-wrap">{item.text}</pre>
            )}
          </div>
        )}
        {confirmDeleteThinking && (
          <ConfirmDialog
            title="Delete thinking"
            message="The model's reasoning will be removed from this message. This cannot be undone."
            confirmLabel="Delete"
            onConfirm={this.confirmThinkingDelete}
            onCancel={this.cancelDeleteThinking}
          />
        )}
      </div>
    );
  }

  /**
   * Renders a tool call/result row.
   *
   * @param item - the tool item
   * @param jobCompletions - async job outcomes keyed by tool call id
   */
  private renderTool(item: ToolActionItem, jobCompletions: ReadonlyMap<string, AsyncJobCompletion>) {
    const open = this.isOpen();
    const call = item.callPart;
    const result = item.resultPart;
    const toolCallID = toolCallIDOf(item);
    const name = call?.Name ?? result?.Name ?? "tool";
    const subject = call ? formatActionArgs(call.Name, call.Input) : "";
    // `result` (paired by ToolCallID) is the real "tool returned" signal.
    // call.Finished is NOT: it means the model finished typing the
    // arguments and is set before the tool is dispatched
    // (internal/agent/agent_turn.go OnToolInputEnd/OnToolCall), so keying
    // on it flashed the badge for a blink while args streamed and hid it
    // for the whole execution window.
    const completion = jobCompletions.get(call?.ID ?? result?.ToolCallID ?? "");
    const pendingJob = isPendingJob(result?.Metadata);
    const running = !!call && (!result || (pendingJob && !completion));
    const errored = !!result?.IsError || completion?.status === "failed";
    const completed = pendingJob && completion?.status === "finished";
    // Stage 5b: the terminal-cause badges — timed out / stopped by the user
    // (batch_stop) / cancelled (session stop) must be distinguishable from a
    // plain failure, sourced from the completion notice the client already
    // received (asyncStoppedPattern), never synthesized from message text
    // shown as a user reply.
    return (
      <div data-test-id="action-row" data-tool-call-id={toolCallID} className="action-row">
        <button
          type="button"
          onClick={this.toggle}
          aria-expanded={open}
          data-test-id="action-row-toggle"
          className="action-row-head"
          title={subject || name}
        >
          <span className="text-xs text-text-subtle shrink-0">⚡</span>
          <span className="text-mauve font-semibold text-sm shrink-0">{name}</span>
          {/* Subject (file path, command, pattern, …) is the primary readable
              label of the row — same size and weight as the tool name so a
              collapsed accordion immediately tells the operator WHICH file
              each action touched, not just that "an edit happened". */}
          <span className="text-text font-mono text-sm truncate flex-1 min-w-0">
            {subject || "—"}
          </span>
          {item.repeatCount && item.repeatCount > 1 && (
            <span className="px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px] shrink-0">
              ×{item.repeatCount}
            </span>
          )}
          {running && <span className="text-text-subtle text-xs animate-pulse shrink-0">running…</span>}
          {completion?.status === "timed_out" && (
            <span data-test-id="action-row-status" className="text-xs text-yellow font-medium shrink-0">timed out</span>
          )}
          {completion?.status === "stopped" && (
            <span data-test-id="action-row-status" className="text-xs text-text-subtle font-medium shrink-0">
              stopped by user
            </span>
          )}
          {completion?.status === "cancelled" && (
            <span data-test-id="action-row-status" className="text-xs text-text-subtle font-medium shrink-0">
              cancelled
            </span>
          )}
          {errored && <span className="badge-error shrink-0">error</span>}
          {completed && <span className="text-xs text-green font-medium shrink-0">done</span>}
          <TimeBadge epochSec={item.createdAt} />
          <span className="text-text-subtle shrink-0">
            {open ? <ChevronUp size={14} /> : <ChevronDown size={14} />}
          </span>
        </button>
        {open && (
          <div className="action-row-body">
            {call && <ToolCallBlock name={call.Name} input={call.Input} running={running} />}
            {result && (
              <ToolResultBlock
                name={result.Name}
                content={completion?.content ?? result.Content}
                isError={errored}
                metadata={result.Metadata}
              />
            )}
          </div>
        )}
      </div>
    );
  }

  /** Renders a thinking row, or a tool row bound to the async job outcomes. */
  public render() {
    const { item } = this.props;
    if (item.kind === "thinking") return this.renderThinking(item);
    const { byToolCallID } = this.useComputed(asyncJobCompletions);
    return this.renderTool(item, byToolCallID);
  }
}
