// Routes a single message content part to its renderer.
// Pure code move from the former components/Message.tsx.

import { memo } from "react";
import type { ContentPart } from "../../types";
import { SubAgentBlock } from "../SubAgentBlock";
import { AskQuestionBlock } from "./AskQuestionBlock";
import { FinishErrorBlock } from "./FinishErrorBlock";
import { StreamPausedBlock } from "./StreamPausedBlock";
import { TextBlock } from "./TextBlock";
import { ThinkingPart } from "./ThinkingPart";
import { ToolCallBlock } from "./ToolCallBlock";
import { ToolResultBlock } from "./ToolResultBlock";
import { parseAwaitingAnswer } from "./parseAwaitingAnswer";

// ── Part router ───────────────────────────────────────────────────────────────

export const Part = memo(function Part({ part, index, isUser, messageID, thinkingDone, partialWorkDone, model, effort, sessionID }: { part: ContentPart; index: number; isUser: boolean; messageID: string; thinkingDone: boolean; partialWorkDone: boolean; model?: string; effort?: string; sessionID?: string }) {
  switch (part.type) {
    case "text":     return <TextBlock text={part.Text} isUser={isUser} />;
    case "thinking": return <ThinkingPart thinking={part.Thinking} messageID={messageID} partIndex={index} done={thinkingDone} model={model} effort={effort} />;
    case "tool_call": {
      if (part.Name === "agent") {
        let prompt = "";
        try { prompt = JSON.parse(part.Input).prompt ?? part.Input; } catch { prompt = part.Input; }
        return <SubAgentBlock messageID={messageID} toolCallID={part.ID} prompt={prompt} finished={part.Finished} />;
      }
      return <ToolCallBlock name={part.Name} input={part.Input} finished={part.Finished} />;
    }
    case "tool_result": {
      if (part.Name === "agent") return null;
      return <ToolResultBlock name={part.Name} content={part.Content} isError={part.IsError} metadata={part.Metadata} />;
    }
    // Fork patch: render explicit error/empty finish parts so a failed turn is
    // never silently rendered as a blank block. See CHANGELOG.fork.md.
    case "finish": {
      // Stream stalled AFTER substantive work happened (tool calls, text,
      // reasoning) is not a real error — the model did useful things, the
      // provider just went quiet on the tail and the watchdog cut the stream.
      // Render that case as a soft amber "paused" notice so the user sees the
      // work above as legitimate, not framed inside a scary red failure block.
      //
      // CROSS-LANGUAGE SYNC: this "Stream stalled" literal mirrors
      // Go-side `streamStalledFinishTitle` in
      // internal/agent/coordinator.go — the single source of truth
      // that agent.Run writes and the retry logic matches against.
      // TypeScript can't import the Go constant, so the two are kept
      // in sync by comment cross-reference, not via the WS/JSON
      // protocol. If the Go literal ever changes, update it here too,
      // or this paused-block branch silently stops matching and a
      // stalled turn renders as a red FinishErrorBlock instead.
      if (part.Reason === "error" && part.Message === "Stream stalled" && partialWorkDone) {
        return <StreamPausedBlock details={part.Details} />;
      }
      const asked = parseAwaitingAnswer(part.Reason, part.Message, part.Details);
      if (asked) {
        return <AskQuestionBlock question={asked.question} options={asked.options} sessionID={sessionID ?? ""} />;
      }
      if (part.Reason === "error" || part.Reason === "canceled") {
        return <FinishErrorBlock reason={part.Reason} message={part.Message} details={part.Details} />;
      }
      return null;
    }
    default: return null;
  }
});
