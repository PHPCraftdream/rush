// Assistant-message content area: block grouping, tool activity groups and
// empty-response handling. Pure code move from the former components/Message.tsx.

import { useMemo, memo } from "react";
import { useStore } from "@nanostores/react";
import type { Message as Msg } from "../../types";
import { $busySessions, $messageBlockBreaks } from "../../store";
import { EditForm } from "./EditForm";
import { FinishErrorBlock } from "./FinishErrorBlock";
import { Part } from "./Part";
import { ToolActivityGroup } from "./ToolActivityGroup";
import { EMPTY_BREAKS, groupPartsIntoBlocks } from "./blocks";
import { extractText } from "./textParts";

export const AssistantContent = memo(function AssistantContent({
  message, editing, onSaveEdit, onCancelEdit,
}: {
  message: Msg;
  editing: boolean;
  onSaveEdit: (text: string) => void;
  onCancelEdit: () => void;
}) {
  const breakMap = useStore($messageBlockBreaks);
  const breaks = useMemo(() => breakMap.get(message.ID) ?? EMPTY_BREAKS, [breakMap, message.ID]);
  const blocks = useMemo(() => groupPartsIntoBlocks(message.Parts, breaks), [message.Parts, breaks]);
  const busy = useStore($busySessions);

  // Fork patch: detect assistant messages that produced no visible content
  // (no text / tool_call / tool_result / thinking). This used to render as a
  // blank block in the WUI. We now show an explicit "empty response" notice
  // for finished turns and a "streaming…" placeholder for live turns.
  const hasVisibleContent = useMemo(
    () => message.Parts.some(p =>
      p.type === "text" || p.type === "tool_call" || p.type === "tool_result" || p.type === "thinking"
    ),
    [message.Parts]
  );
  const isFinished = useMemo(() => message.Parts.some(p => p.type === "finish"), [message.Parts]);
  const finishPart = useMemo(
    () => message.Parts.find(p => p.type === "finish") as (typeof message.Parts[number] & { type: "finish"; Reason: string; Message: string; Details: string }) | undefined,
    [message.Parts]
  );

  if (editing) {
    return (
      <EditForm
        initialValue={extractText(message.Parts)}
        rows={4}
        className="field-textarea text-[16px]"
        onSave={onSaveEdit}
        onCancel={onCancelEdit}
      />
    );
  }
  if (!hasVisibleContent) {
    // A turn that's still in flight may already carry a finish-part in the DB
    // (created speculatively by recovery / cancel paths and rewritten the
    // moment the first real delta arrives). Suppressing the "Empty response"
    // notice while busy avoids the flash where the placeholder shows for a
    // few hundred milliseconds and then disappears under the actual answer.
    const isLive = busy.has(message.SessionID);
    if (isFinished && !isLive) {
      const reason = finishPart?.Reason ?? "unknown";
      const msg = finishPart?.Message || "Empty response";
      const details = finishPart?.Details || "The provider closed the stream without returning any content. Please retry.";
      return (
        <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
          <FinishErrorBlock reason={reason} message={msg} details={details} />
        </div>
      );
    }
    return (
      <div className="text-text-subtle leading-relaxed italic" style={{ fontSize: "var(--chat-font-size)" }}>
        {isLive ? "streaming…" : "(no content)"}
      </div>
    );
  }
  // partialWorkDone — used by the finish-part router to pick StreamPausedBlock
  // (soft amber) over FinishErrorBlock (red) when the watchdog stall happened
  // after the model already produced something substantive.
  const partialWorkDone = hasVisibleContent;
  const isLive = busy.has(message.SessionID);
  return (
    <div className="text-text leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
      {blocks.map((block, bi) => (
        <div key={bi} className={bi > 0 ? "msg-block-sep" : undefined}>
          {block.kind === "tool" ? (
            // Whole tool burst is rendered through the accordion group: one
            // collapsible row per call+result pair, last row open by default
            // (the "current action"), prior rows collapsed. User can pin any
            // row open/closed and the auto-rule stops touching that row.
            //
            // isCurrent={false} preserves today's behaviour EXACTLY: the prop
            // was never passed here, so it arrived as undefined and the
            // auto-rule read it as falsy — every group rendered through this
            // path collapses. Note that contradicts the paragraph above, so
            // it is very likely not what was intended; changing it is a
            // visible UI change that needs to be looked at rather than
            // guessed at from a type error. Made explicit so the typecheck
            // passes without altering what users see.
            <ToolActivityGroup items={block.items.map((it) => ({ ...it, messageID: message.ID }))} live={isLive} isCurrent={false} model={message.Model} effort={message.ReasoningEffort} />
          ) : (
            block.items.map(({ part, idx }) => (
              <Part key={idx} part={part} index={idx} isUser={false} messageID={message.ID} thinkingDone={block.thinkingDone} partialWorkDone={partialWorkDone} model={message.Model} effort={message.ReasoningEffort} sessionID={message.SessionID} />
            ))
          )}
        </div>
      ))}
    </div>
  );
});
