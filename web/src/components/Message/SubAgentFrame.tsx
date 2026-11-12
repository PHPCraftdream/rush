// The collapsible frame of a sub-agent block: header badges and the transcript body.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { Bot } from "lucide-react";
import { messagesCarburetor } from "../../state/messages";
import { sessionsCarburetor } from "../../state/sessions";
import { sendLoadMessages } from "../../ws";
import type { AsyncJobStatus } from "../../asyncJobCompletion";
import type { Message, FinishPart } from "../../types";
import { EffortBadge } from "./EffortBadge";
import { FinishErrorBlock } from "./FinishErrorBlock";
import { SubAgentMessage } from "./SubAgentMessage";
import { SummaryMessage } from "./SummaryMessage";
import { watchExpandToolCallSignal } from "./watchExpandToolCallSignal";

/** Props of the sub-agent frame. */
interface ISubAgentFrameProps {
  toolCallID: string;
  subSessionID: string;
  /** Owning session of the parent message, when the parent is in the transcript. */
  ownerSessionID: string | undefined;
  messages: readonly TReadonly<Message>[];
  completedCallIDs: Set<string>;
  model: string;
  effort: string;
  label: string;
  done: boolean;
  asyncStatus: AsyncJobStatus | undefined;
  errorFinish: TReadonly<FinishPart> | undefined;
}

/** Local state of the sub-agent frame. */
interface ISubAgentFrameState {
  // Open while the sub-agent is still working (mirrors prior `open={!done}`
  // behaviour); the user's manual toggle wins once they touch the chevron.
  override: boolean | undefined;
}

/**
 * Whether a sub-agent message is shown: assistant prose that is not hidden
 * (silent compaction summaries are dropped).
 *
 * @param m - a sub-agent message
 */
const isShownMessage = (m: TReadonly<Message>): boolean => m.Role === "assistant" && !m.Hidden;

/** Header badges plus the collapsible sub-agent transcript. */
export class SubAgentFrame extends AntiHookComponent<ISubAgentFrameProps, ISubAgentFrameState> {
  public state: ISubAgentFrameState = { override: undefined };

  private prevDone = this.props.done;

  // Latch on the session ID rather than a bare boolean: a boolean latch cannot
  // distinguish "already asked for THIS session" from "already asked for SOME
  // session", so a reused instance handed a different subSessionID would never
  // load. Latching on the value itself is correct independently of how callers key us.
  private requested: string | null = null;

  /** Declares the lazy-load, done-reset and jump-to-tool-call effects. */
  protected useEffects(): void {
    super.useEffects();
    const { subSessionID, messages, ownerSessionID, done, toolCallID } = this.props;
    this.useEffect(this.requestMessages, [subSessionID, messages.length, ownerSessionID]);
    this.useEffect(this.resetOverrideOnDone, [done]);
    this.useEffect(this.watchExpand, [toolCallID]);
  }

  /**
   * Lazy-loads the sub-agent messages on first mount when the store has none.
   *
   * The WS handler only auto-loads sub-sessions created during the
   * live session — past runs surfaced after a reload start empty). Registers
   * before asking: the messages_list router (useWS.ts) only routes replies for
   * sessions present in $subAgentSessions. After a reload a historical
   * sub-session is registered only if its session_created survived the hub's
   * replay ring — otherwise the reply fell through to the main-chat branch and
   * was silently dropped, leaving this block empty forever. The owning session
   * is the parent message's own SessionID.
   */
  @bind
  protected requestMessages(): void {
    const { subSessionID, messages, ownerSessionID } = this.props;
    if (this.requested === subSessionID) return;
    if (messages.length > 0) return;
    this.requested = subSessionID;
    const owner = ownerSessionID ?? sessionsCarburetor.getData().activeSessionID;
    if (owner) messagesCarburetor.registerSubAgentSession(subSessionID, owner);
    sendLoadMessages(subSessionID);
  }

  /** Drops the user's pin when the run finishes, so the block follows the auto-rule again. */
  @bind
  protected resetOverrideOnDone(): void {
    const { done } = this.props;
    if (done && !this.prevDone) this.setState({ override: undefined });
    this.prevDone = done;
  }

  /** Subscribes to jump-to-tool-call requests (task #1059) so the Agents tab can force this block open. */
  @bind
  protected watchExpand(): () => void {
    return watchExpandToolCallSignal(this.props.toolCallID, this.forceOpen);
  }

  /** Pins the block open. */
  @bind
  protected forceOpen(): void {
    this.setState({ override: true });
  }

  /** Whether the body is open: the user's pin, else open while the run is not done. */
  private isOpen(): boolean {
    return this.state.override ?? !this.props.done;
  }

  /** Flips the body. */
  @bind
  protected toggle(): void {
    this.setState({ override: !this.isOpen() });
  }

  /**
   * Renders one sub-agent message; visible compaction summaries render via the
   * same SummaryMessage card as the main renderer.
   *
   * @param m - the sub-agent message
   */
  @bind
  protected renderMessage(m: TReadonly<Message>) {
    return m.IsSummaryMessage ? (
      <SummaryMessage key={m.ID} message={m} />
    ) : (
      <SubAgentMessage key={m.ID} message={m} completed={this.props.completedCallIDs} />
    );
  }

  /**
   * Renders the terminal-status badge of a finished run.
   *
   * Stage 5b: the
   * delegation's terminal cause must be distinguishable, same rule as the
   * command accordion's ActionRow — timed out / stopped by the parent
   * (stop_agent) / cancelled (session stop) must not read as a plain child
   * failure. A terminal error finish on the child transcript still wins: it is
   * the richer signal.
   */
  private renderDoneBadge() {
    const { errorFinish, asyncStatus } = this.props;
    if (errorFinish || asyncStatus === "failed") return <span className="text-xs text-red font-medium">error</span>;
    if (asyncStatus === "timed_out" || asyncStatus === "stopped" || asyncStatus === "cancelled") {
      return (
        <span data-test-id="sub-agent-status" className="text-xs text-text-subtle font-medium shrink-0">
          {asyncStatus === "timed_out" ? "timed out" : asyncStatus === "stopped" ? "stopped by user" : "cancelled"}
        </span>
      );
    }
    return <span className="text-xs text-green font-medium">done</span>;
  }

  /** Renders the toggle header and, while open, the transcript. */
  public render() {
    const { toolCallID, messages, model, effort, label, done, errorFinish } = this.props;
    const open = this.isOpen();
    const isRunning = !done;

    return (
      <div className="sub-agent-block my-2" data-tool-call-id={toolCallID}>
        <button
          type="button"
          onClick={this.toggle}
          aria-expanded={open}
          className="sub-agent-toggle w-full text-left bg-transparent border-0"
        >
          <Bot size={15} className={`shrink-0 ${isRunning ? "text-accent animate-pulse" : "text-text-subtle"}`} />
          <span className="font-semibold text-sm">Agent</span>
          {model && <span className="text-xs text-text-subtle font-mono shrink-0">{model}</span>}
          <EffortBadge effort={effort} />
          {isRunning && <span className="text-xs text-text-subtle animate-pulse">running...</span>}
          {done && this.renderDoneBadge()}
          <span className="text-xs text-text-muted truncate ml-1 max-w-[400px]">{label}</span>
        </button>
        {open && (
          <div className="sub-agent-body">
            {messages.length === 0 && isRunning && (
              <div className="text-xs text-text-subtle animate-pulse py-2">Starting agent...</div>
            )}
            {/* Lifecycle parity with the main renderer (Message/Message.tsx):
                hidden messages (silent compaction summaries) are dropped,
                visible compaction summaries render via the same SummaryMessage
                card — previously both rendered here as ordinary agent prose. */}
            {messages.filter(isShownMessage).map(this.renderMessage)}
            {errorFinish && (
              <FinishErrorBlock
                reason={errorFinish.Reason}
                message={errorFinish.Message}
                details={errorFinish.Details}
              />
            )}
          </div>
        )}
      </div>
    );
  }
}
