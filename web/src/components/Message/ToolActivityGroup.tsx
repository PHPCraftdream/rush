// Accordion group for a burst of tool/thinking parts between text stretches.
// Exported as part of the Message module's public surface.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { ChevronDown, ChevronUp } from "lucide-react";
import { signalsCarburetor } from "../../state/signals";
import type { ExpandToolCallRequest, ISignalsState } from "../../state/SignalsCarburetor";
import { SubAgentBlock } from "../SubAgentBlock";
import { ActionRow } from "./ActionRow";
import type { ActionItem } from "./ActionRow";
import { TimeBadge } from "./TimeBadge";
import { agentPrompt } from "./agentPrompt";
import { groupToolActions } from "./groupToolActions";
import type { IGroupSourceItem, IRawAgentPart } from "./groupToolActions";
import { tallyActions } from "./tallyActions";
import { watchCollapseAllSignal } from "./watchCollapseAllSignal";

// ── Tool activity group ───────────────────────────────────────────────────────
//
// A "tool" visual block (a burst of tool_call/tool_result parts between two
// stretches of text or thinking) gets rendered as a vertical accordion list
// instead of a flat stack of cards. Rules:
//
//   • One row per pair: a tool_call is paired with its tool_result by
//     ToolCallID. Unpaired results (rare — orphaned from an aborted earlier
//     call) get their own row with no head.
//
//   • Row open-state: `open = userOverride ?? isCurrent`.
//     isCurrent = last row in the list. While the turn is live, that's the
//     in-flight action. After the turn ends, the last action stays expanded
//     by default (most relevant to look at), prior rows stay collapsed.
//     A click freezes the override and the auto-rule no longer touches it.
//
//   • Group open-state: open by default (user wanted to "see the process").
//     Manual collapse via the chevron in the header — never auto-collapsed.

/** Props of the tool activity group. */
interface IToolActivityGroupProps {
  items: IGroupSourceItem[];
  live: boolean;
  // True when this group is the most recent activity in the transcript
  // (i.e. nothing rendered after it). When false, the auto-rule collapses
  // the group — the user moved on, the work is in the past.
  isCurrent: boolean;
  startedAt?: number;
  model?: string;
  effort?: string;
}

/**
 * Group open/close state machine.
 *
 * The default collapsed state follows `isCurrent`: the most recent group stays
 * expanded, older groups fold automatically as soon as a newer renderitem
 * (typically a user message or the next group) appears in the transcript. The
 * user's own toggle wins via `collapsedOverride` and sticks until explicitly
 * changed.
 *
 * `suppressAuto` is a one-shot latch for the inside-of-the-group auto-current
 * rule: after a manual collapse, re-expanding the group leaves EVERY row closed
 * (including the row that would normally auto-open because it's the last one).
 * The latch is released the moment a NEW tool arrives within the same group —
 * that's a live event the user wants to see — or when the group transitions
 * back to isCurrent.
 *
 * When the body is unmounted (`collapsed === true`) every ActionRow's internal
 * override state is reset (React unmount discards component state), so
 * collapsing the group also drops any per-row pins the user had set —
 * "сворачивание идёт с уничтожением контента" verbatim.
 */
interface IToolActivityGroupState {
  collapsedOverride: boolean | undefined;
  suppressAuto: boolean;
  // Sticky-current latch. While the session is `live` (busy), buildRenderItems
  // can flicker isCurrent off for a frame whenever a streaming assistant
  // message is briefly tool-less (thinking-only). Letting that flicker
  // collapse the container would unmount its body and lose all per-row
  // pins/state. So:
  //   - isCurrent === true  → latch to true.
  //   - isCurrent === false AND live === true → keep prior latch value.
  //   - isCurrent === false AND live === false → release latch (true → false).
  // Once the agent finishes (live = false) the natural !isCurrent auto-collapse
  // kicks in again and old groups fold.
  stickyCurrent: boolean;
}

/**
 * Picks the expand-tool-call request out of the signals state.
 *
 * @param state - the signals state
 */
const selectExpandRequest = (state: TReadonly<ISignalsState>) => state.expandToolCallRequest;

/**
 * Flips the group's collapsed state, resolving the auto-rule when no pin exists.
 *
 * @param prev - the previous state
 */
const flipCollapsed = (prev: IToolActivityGroupState): Pick<IToolActivityGroupState, "collapsedOverride"> => ({
  collapsedOverride: !(prev.collapsedOverride ?? !prev.stickyCurrent),
});

/**
 * Renders one sub-agent part of the group.
 *
 * @param agent - the held-back agent part
 * @param agent.part - the message part
 * @param agent.messageID - the owning message
 */
const renderAgent = ({ part, messageID }: IRawAgentPart) => {
  if (part.type !== "tool_call") return null;
  return (
    <SubAgentBlock
      key={`a-${part.ID}`}
      messageID={messageID ?? ""}
      toolCallID={part.ID}
      prompt={agentPrompt(part.Input)}
    />
  );
};

/** Accordion of paired tool call/result rows, with its own collapse state machine. */
export class ToolActivityGroup extends AntiHookComponent<IToolActivityGroupProps, IToolActivityGroupState> {
  public state: IToolActivityGroupState = {
    collapsedOverride: undefined,
    suppressAuto: false,
    stickyCurrent: this.props.isCurrent,
  };

  private prevItemsLen = this.props.items.length;

  private prevIsCurrent = this.props.isCurrent;

  /** Declares the latch-sync, live-event, auto-collapse and signal effects. */
  protected useEffects(): void {
    super.useEffects();
    const { items, isCurrent, live } = this.props;
    this.useEffect(this.syncStickyCurrent, [isCurrent, live]);
    this.useEffect(this.releaseOnLiveEvent, [items.length, isCurrent]);
    this.useEffect(this.latchSuppress, [this.isCollapsed()]);
    this.useEffect(this.watchSignals, []);
  }

  /** Whether the group body is currently folded: the user's pin, else the auto-rule. */
  private isCollapsed(): boolean {
    const { collapsedOverride, stickyCurrent } = this.state;
    return collapsedOverride ?? !stickyCurrent;
  }

  /** Latches the group as current while live; releases the latch once the turn ends. */
  @bind
  protected syncStickyCurrent(): void {
    const { isCurrent, live } = this.props;
    if (isCurrent) this.setState({ stickyCurrent: true });
    else if (!live) this.setState({ stickyCurrent: false });
  }

  /**
   * Drops the collapse pin and the suppress latch on a live event.
   *
   * A new tool arrived in this group or this group just became the latest
   * activity: a live event takes precedence, and the new last row auto-opens via its isCurrent
   * prop in ActionRow.
   */
  @bind
  protected releaseOnLiveEvent(): void {
    const { items, isCurrent } = this.props;
    const grew = items.length > this.prevItemsLen;
    const becameCurrent = isCurrent && !this.prevIsCurrent;
    if (grew || becameCurrent) this.setState({ collapsedOverride: undefined, suppressAuto: false });
    this.prevItemsLen = items.length;
    this.prevIsCurrent = isCurrent;
  }

  // Any collapse (manual OR automatic via isCurrent → false) latches
  // suppressAuto. The body unmounts, which already discards each ActionRow's
  // override state; setting the latch ensures that on the next expand the
  // auto-current rule doesn't re-open the last row either — every row stays
  // closed until the user clicks one, or a fresh tool arrival clears the
  // latch via releaseOnLiveEvent.
  /** Latches suppressAuto whenever the group is collapsed. */
  @bind
  protected latchSuppress(): void {
    if (this.isCollapsed()) this.setState({ suppressAuto: true });
  }

  /** Subscribes to the global collapse-all and jump-to-tool-call signals; returns the disposer for both. */
  @bind
  protected watchSignals(): () => void {
    const stopCollapse = watchCollapseAllSignal(this.collapseAll);
    const stopExpand = signalsCarburetor.watch(selectExpandRequest, this.onExpandRequest);
    return () => { stopCollapse(); stopExpand(); };
  }

  /** Folds the group body ("Collapse all"). */
  @bind
  protected collapseAll(): void {
    this.setState({ collapsedOverride: true });
  }

  /**
   * Forces the group open when a jump request targets a tool call it contains.
   *
   * Jump-to-tool-call (task #1059): the LiveWorkPanel asks to expand a
   * tool call this burst contains; the group opens regardless of the
   * auto-current rule -- unlike releaseOnLiveEvent, `undefined` isn't enough
   * here: an older, non-current group's autoCollapsed would still resolve to
   * true. Opening the group only ensures its body (and every ActionRow /
   * SubAgentBlock in it) actually MOUNTS -- each of those self-opens via its
   * own expand-signal subscription once it exists, since the whole body
   * unmounts while collapsed.
   *
   * @param req - the new request, if any
   */
  @bind
  protected onExpandRequest(req: TReadonly<ExpandToolCallRequest> | null): void {
    if (!req || !this.containsToolCall(req.toolCallID)) return;
    this.setState({ collapsedOverride: false, suppressAuto: false });
  }

  /**
   * Whether the burst holds the tool call (or its result).
   *
   * @param id - the tool call id
   */
  private containsToolCall(id: string): boolean {
    for (const { part } of this.props.items) {
      if (part.type === "tool_call" && part.ID === id) return true;
      if (part.type === "tool_result" && part.ToolCallID === id) return true;
    }
    return false;
  }

  /** Flips the group body. */
  @bind
  protected toggle(): void {
    this.setState(flipCollapsed);
  }

  /**
   * Renders one accordion row; the last row is the "current action".
   *
   * @param a - the row item
   * @param i - the row index
   * @param all - every row
   */
  @bind
  protected renderRow(a: ActionItem, i: number, all: readonly ActionItem[]) {
    const { model, effort } = this.props;
    return (
      <ActionRow
        key={a.key}
        item={a}
        isCurrent={i === all.length - 1}
        suppressAutoCurrent={this.state.suppressAuto}
        // Per-row model/effort from the part's own source message (burst path);
        // falls back to group-level props (single-message path via AssistantContent).
        model={a.model || model}
        effort={a.effort || effort}
      />
    );
  }

  /** Renders nothing, the inline row/agents, or the full accordion. */
  public render() {
    const { items, live, startedAt, model, effort } = this.props;
    const collapsed = this.isCollapsed();
    const { actions, rawAgentParts } = groupToolActions(items);
    const tally = tallyActions(actions);

    // Nothing to show — neither real actions nor sub-agent calls. Without this
    // guard buildRenderItems-driven empty bursts would render a stray "0 actions"
    // header with an empty body.
    if (actions.length === 0 && rawAgentParts.length === 0) return null;

    // Group contained ONLY agent tool_calls (e.g. fan-out steps). The accordion
    // header is meaningless ("0 actions"), so render the sub-agent blocks
    // inline without the group wrapper.
    // SubAgent parts always render in their own (existing) component, in their
    // original order at the END of the group so they don't get swallowed by the
    // collapse toggle. They're rare in practice — usually zero per group.
    if (actions.length === 0) {
      return <>{rawAgentParts.map(renderAgent)}</>;
    }

    // Single action with no sub-agent — the "1 action" accordion header would
    // just duplicate the row's own header. Render the row inline so the
    // operator sees one thing, not two. (When sub-agents are present we keep
    // the wrapper because renderAgents adds another item below.)
    if (actions.length === 1 && rawAgentParts.length === 0) {
      return (
        <ActionRow
          key={actions[0].key}
          item={actions[0]}
          isCurrent={this.state.stickyCurrent}
          suppressAutoCurrent={false}
          // Per-row model/effort from the part's own source message (burst path);
          // falls back to group-level props (single-message path via AssistantContent).
          model={actions[0].model || model}
          effort={actions[0].effort || effort}
        />
      );
    }

    return (
      <div data-test-id="tool-activity-group" className="tool-activity-group">
        <button
          type="button"
          onClick={this.toggle}
          data-test-id="tool-activity-toggle"
          className="tool-activity-head"
          aria-expanded={!collapsed}
          title={collapsed ? "Expand actions" : "Collapse actions"}
        >
          <span className="text-mauve font-semibold text-sm shrink-0">
            {actions.length} {actions.length === 1 ? "action" : "actions"}
          </span>
          {tally && <span className="text-text-subtle text-xs truncate flex-1 min-w-0">{tally}</span>}
          {!tally && <span className="flex-1" />}
          <TimeBadge epochSec={startedAt} />
          {live && <span className="text-text-subtle text-xs animate-pulse shrink-0">live</span>}
          <span className="text-text-subtle shrink-0">
            {collapsed ? <ChevronDown size={16} /> : <ChevronUp size={16} />}
          </span>
        </button>
        {!collapsed && (
          <div className="tool-activity-body">
            {actions.map(this.renderRow)}
            {rawAgentParts.map(renderAgent)}
            {/* Sticky bottom collapse strip, same affordance as ThinkingPart:
                long tool bursts are tedious to fold when the toggle is only
                at the top. Reuses the .thinking-collapse-bottom style. */}
            <button
              type="button"
              onClick={this.toggle}
              data-test-id="tool-activity-collapse-bottom"
              className="thinking-collapse-bottom"
              title="Collapse actions"
            >
              <ChevronUp size={14} />
              <span>Collapse actions</span>
            </button>
          </div>
        )}
      </div>
    );
  }
}
