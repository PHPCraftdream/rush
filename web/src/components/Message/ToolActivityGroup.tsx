// Accordion group for a burst of tool/thinking parts between text stretches.
// Exported as part of the Message module's public surface.
// Pure code move from the former components/Message.tsx.

import { useState, useRef, useEffect, useMemo, useCallback, memo } from "react";
import { ChevronDown, ChevronUp } from "lucide-react";
import type { ContentPart } from "../../types";
import { SubAgentBlock } from "../SubAgentBlock";
import { ActionRow } from "./ActionRow";
import type { ActionItem } from "./ActionRow";
import { TimeBadge } from "./TimeBadge";
import { useCollapseAllSignal } from "./useCollapseAllSignal";

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

interface ToolActivityGroupProps {
  items: { part: ContentPart; idx: number; createdAt?: number; messageID?: string }[];
  live: boolean;
  // True when this group is the most recent activity in the transcript
  // (i.e. nothing rendered after it). When false, the auto-rule collapses
  // the group — the user moved on, the work is in the past.
  isCurrent: boolean;
  startedAt?: number;
  model?: string;
  effort?: string;
}

export const ToolActivityGroup = memo(function ToolActivityGroup({ items, live, isCurrent, startedAt, model, effort }: ToolActivityGroupProps) {
  // Group open/close state machine.
  //
  // The default collapsed state follows `isCurrent`: the most recent group
  // stays expanded, older groups fold automatically as soon as a newer
  // renderitem (typically a user message or the next group) appears in the
  // transcript. The user's own toggle wins via `collapsedOverride` and
  // sticks until explicitly changed.
  //
  // `suppressAuto` is a one-shot latch for the inside-of-the-group auto-
  // current rule: after a manual collapse, re-expanding the group leaves
  // EVERY row closed (including the row that would normally auto-open
  // because it's the last one). The latch is released the moment a NEW
  // tool arrives within the same group — that's a live event the user
  // wants to see — or when the group transitions back to isCurrent.
  //
  // When the body is unmounted (`collapsed === true`) every ActionRow's
  // internal override state is reset (React unmount discards useState), so
  // collapsing the group also drops any per-row pins the user had set —
  // "сворачивание идёт с уничтожением контента" verbatim.
  const [collapsedOverride, setCollapsedOverride] = useState<boolean | undefined>(undefined);
  const [suppressAuto, setSuppressAuto] = useState(false);
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
  const [stickyCurrent, setStickyCurrent] = useState(isCurrent);
  useEffect(() => {
    if (isCurrent) setStickyCurrent(true);
    else if (!live) setStickyCurrent(false);
  }, [isCurrent, live]);
  const effectiveCurrent = stickyCurrent;
  const autoCollapsed = !effectiveCurrent;
  const collapsed = collapsedOverride ?? autoCollapsed;
  useCollapseAllSignal(() => setCollapsedOverride(true));

  const prevItemsLen = useRef(items.length);
  const prevIsCurrent = useRef(isCurrent);
  useEffect(() => {
    const grew = items.length > prevItemsLen.current;
    const becameCurrent = isCurrent && !prevIsCurrent.current;
    if (grew || becameCurrent) {
      // Either a new tool arrived in this group, or this group just became
      // the latest activity. Drop the user's collapse pin (live event takes
      // precedence) and clear the suppress latch so the new last row auto-
      // opens via its isCurrent prop in ActionRow.
      setCollapsedOverride(undefined);
      setSuppressAuto(false);
    }
    prevItemsLen.current = items.length;
    prevIsCurrent.current = isCurrent;
  }, [items.length, isCurrent]);

  // Any collapse (manual OR automatic via isCurrent → false) latches
  // suppressAuto. The body unmounts, which already discards each ActionRow's
  // override state; setting the latch ensures that on the next expand the
  // auto-current rule doesn't re-open the last row either — every row stays
  // closed until the user clicks one, or a fresh tool arrival clears the
  // latch via the effect above.
  useEffect(() => {
    if (collapsed) setSuppressAuto(true);
  }, [collapsed]);

  const toggle = useCallback(() => {
    setCollapsedOverride((prev) => {
      const cur = prev ?? autoCollapsed;
      return !cur;
    });
  }, [autoCollapsed]);

  // Pair calls and results by ToolCallID. Order is the order calls appear.
  // An "agent" tool_call is passed through unchanged — SubAgentBlock owns
  // its own rendering and lives outside the row schema. Same for the matching
  // tool_result if any.
  const { actions, rawAgentParts } = useMemo(() => {
    const actions: ActionItem[] = [];
    const rawAgentParts: { part: ContentPart; idx: number; messageID?: string }[] = [];
    const indexByCallID = new Map<string, number>();
    for (const { part, idx, createdAt, messageID } of items) {
      if (part.type === "thinking") {
        const text = (part as { type: "thinking"; Thinking: string }).Thinking ?? "";
        actions.push({ kind: "thinking", text, idx, key: `think-${idx}`, createdAt, messageID, partIndex: idx });
      } else if (part.type === "tool_call") {
        if (part.Name === "agent") { rawAgentParts.push({ part, idx, messageID }); continue; }
        const a: ActionItem = { kind: "tool", callPart: part, idx, key: `call-${part.ID}`, createdAt };
        indexByCallID.set(part.ID, actions.length);
        actions.push(a);
      } else if (part.type === "tool_result") {
        if (part.Name === "agent") { rawAgentParts.push({ part, idx, messageID }); continue; }
        const pos = indexByCallID.get(part.ToolCallID);
        if (pos !== undefined) {
          const slot = actions[pos];
          if (slot.kind === "tool") slot.resultPart = part;
          // intentionally keep the earlier createdAt: that's when the action began
        } else {
          actions.push({ kind: "tool", resultPart: part, idx, key: `res-${part.ToolCallID}-${idx}`, createdAt });
        }
      }
    }
    // Dedup consecutive tool actions with identical Name+Input (e.g. repeated
    // job_output polling). Keep the LAST occurrence (freshest result) and
    // annotate it with repeatCount so the UI can show "×N".
    const deduped: ActionItem[] = [];
    for (const a of actions) {
      const prev = deduped[deduped.length - 1];
      if (
        prev &&
        a.kind === "tool" &&
        prev.kind === "tool" &&
        a.callPart &&
        prev.callPart &&
        a.callPart.Name === prev.callPart.Name &&
        a.callPart.Input === prev.callPart.Input
      ) {
        // Replace prev with current (fresher result), bump count
        a.repeatCount = (prev.repeatCount ?? 1) + 1;
        a.createdAt = prev.createdAt;
        deduped[deduped.length - 1] = a;
      } else {
        deduped.push(a);
      }
    }
    return { actions: deduped, rawAgentParts };
  }, [items]);

  const tally = useMemo(() => {
    const counts = new Map<string, number>();
    let thinkingCount = 0;
    for (const a of actions) {
      if (a.kind === "thinking") { thinkingCount++; continue; }
      const n = a.callPart?.Name ?? a.resultPart?.Name ?? "tool";
      counts.set(n, (counts.get(n) ?? 0) + 1);
    }
    const parts = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 4)
      .map(([k, v]) => `${v} ${k}`);
    if (thinkingCount > 0) parts.push(`${thinkingCount} thinking`);
    return parts.join(" · ");
  }, [actions]);

  // SubAgent parts always render in their own (existing) component, in their
  // original order at the END of the group so they don't get swallowed by the
  // collapse toggle. They're rare in practice — usually zero per group.
  const renderAgents = () => rawAgentParts.map(({ part, messageID }) => {
    if (part.type === "tool_call") {
      let prompt = "";
      try { prompt = (JSON.parse(part.Input) as { prompt?: string }).prompt ?? part.Input; } catch { prompt = part.Input; }
      return <SubAgentBlock key={`a-${part.ID}`} messageID={messageID ?? ""} toolCallID={part.ID} prompt={prompt} finished={part.Finished} />;
    }
    return null;
  });

  // Nothing to show — neither real actions nor sub-agent calls. Without this
  // guard buildRenderItems-driven empty bursts would render a stray "0 actions"
  // header with an empty body.
  if (actions.length === 0 && rawAgentParts.length === 0) return null;

  // Group contained ONLY agent tool_calls (e.g. fan-out steps). The accordion
  // header is meaningless ("0 actions"), so render the sub-agent blocks
  // inline without the group wrapper.
  if (actions.length === 0) {
    return <>{renderAgents()}</>;
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
        isCurrent={effectiveCurrent}
        suppressAutoCurrent={false}
        model={model}
        effort={effort}
      />
    );
  }

  return (
    <div data-test-id="tool-activity-group" className="tool-activity-group">
      <button
        type="button"
        onClick={toggle}
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
          {actions.map((a, i) => (
            <ActionRow
              key={a.key}
              item={a}
              isCurrent={i === actions.length - 1}
              suppressAutoCurrent={suppressAuto}
              model={model}
              effort={effort}
            />
          ))}
          {renderAgents()}
          {/* Sticky bottom collapse strip, same affordance as ThinkingPart:
              long tool bursts are tedious to fold when the toggle is only
              at the top. Reuses the .thinking-collapse-bottom style. */}
          <button
            type="button"
            onClick={toggle}
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
});
