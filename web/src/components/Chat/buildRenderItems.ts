import type { TReadonly } from "@react-carburetor/core";
import type { Message as Msg, ContentPart } from "../../types";

/** The slice of a part the grouper inspects before narrowing it. */
interface PartLike {
  type: string;
  Reason?: string;
}

/**
 * One tool/thinking part of a burst, carried with its source message's metadata.
 *
 * BurstPart carries each part alongside its source message's CreatedAt, Model and
 * ReasoningEffort, and its real partIndex (index into the source message's Parts
 * array). Each row in ToolActivityGroup renders its own HH:MM:SS timestamp and
 * model/effort badge — the part itself has none, the message does — and WS commands
 * need the REAL partIndex for server addressing (burst position is a render concern only).
 */
export type BurstPart = {
  part: TReadonly<ContentPart>;
  createdAt: number;
  messageID: string;
  partIndex: number;
  model?: string;
  effort?: string;
};

/** One row of the transcript after cross-message tool grouping. */
export type RenderItem =
  | { kind: "message"; message: TReadonly<Msg>; index: number }
  | { kind: "toolrun"; parts: BurstPart[]; firstMsgID: string }
  | { kind: "standalonetext"; messageID: string; partIndex: number; text: string };

/**
 * Wraps one part with the metadata of the message it came from.
 *
 * @param m - the source message
 * @param part - the part
 * @param partIndex - the part's index in the source message's Parts array
 */
const burstPart = (m: TReadonly<Msg>, part: TReadonly<ContentPart>, partIndex: number): BurstPart => ({
  part,
  createdAt: m.CreatedAt,
  messageID: m.ID,
  partIndex,
  model: m.Model,
  effort: m.ReasoningEffort,
});

/**
 * Whether an assistant message carries tool parts, and whether it ended in error/cancel.
 *
 * @param m - the assistant message
 */
const scanAssistantParts = (m: TReadonly<Msg>): { hasTool: boolean; hasErrorFinish: boolean } => {
  let hasTool = false;
  let hasErrorFinish = false;
  for (const raw of m.Parts) {
    const p = raw as unknown as PartLike;
    if (p.type === "tool_call" || p.type === "tool_result") hasTool = true;
    if (p.type === "finish" && (p.Reason === "error" || p.Reason === "canceled")) hasErrorFinish = true;
  }
  return { hasTool, hasErrorFinish };
};

// buildRenderItems groups consecutive tool activity across messages into
// one cross-message accordion (ToolRun) and renders everything else as a
// normal <Message>. The model emits a short "I'll check the next file"
// preface next to every tool_call, but its information value is already
// in the tool's args (file_path / command / pattern) which the action row
// header surfaces verbatim — so for a tool-bearing assistant message we
// drop the text/thinking parts and keep only the tools. That collapses a
// 250-step turn into ONE accordion with 250 rows, matching the user's
// "merge consecutive ones" requirement.
//
// Rules (one pass over messages):
//   - hidden message              → skip entirely (does NOT break the run)
//   - user / summary              → flush + standalone <Message>
//   - error/canceled finish       → flush + standalone <Message> so the
//                                    StreamPausedBlock / FinishErrorBlock
//                                    renders in the right context
//   - Role === "tool"             → append every tool_result part to the
//                                    burst (tool-role messages carry only
//                                    results, no standalone needed)
//   - assistant w/ any tool_call  → drop text/thinking, append all
//                                    tool_call / tool_result to the burst
//   - assistant w/o tool_call     → flush + standalone <Message> (this is
//                                    the model's final prose answer, or
//                                    a pure-thinking turn)
//
// The agent emits a brand-new assistant message per turn-step, and almost
// every step carries a `thinking` part next to its tool_call. Grouping is
// lifted to the part level: a text part, a user message, an error/canceled
// finish, a summary or hidden message all flush the burst — so the
// accordion always corresponds to one uninterrupted stretch of tool work.

/**
 * Groups consecutive tool activity across messages into cross-message tool runs.
 *
 * @param messages - the transcript
 * @param attachedNoticeIDs - notice message ids that render inside their tool block
 */
export function buildRenderItems(messages: TReadonly<Msg[]>, attachedNoticeIDs: ReadonlySet<string>): RenderItem[] {
  const out: RenderItem[] = [];
  let burstParts: BurstPart[] = [];
  let burstFirstID = "";

  const flushBurst = () => {
    if (burstParts.length === 0) return;
    out.push({ kind: "toolrun", parts: burstParts, firstMsgID: burstFirstID });
    burstParts = [];
    burstFirstID = "";
  };

  messages.forEach((m, i) => {
    if (m.Hidden || attachedNoticeIDs.has(m.ID)) return; // attached results live in their tool block
    if (m.Role === "user" || m.IsSummaryMessage) {
      flushBurst();
      out.push({ kind: "message", message: m, index: i });
      return;
    }

    // Tool-role messages contribute only tool_result parts to the burst.
    if (m.Role === "tool") {
      if (burstFirstID === "") burstFirstID = m.ID;
      for (let pi = 0; pi < m.Parts.length; pi++) {
        const p = m.Parts[pi];
        if (p.type === "tool_result") burstParts.push(burstPart(m, p, pi));
      }
      return;
    }

    // Assistant messages from here on.
    const { hasTool, hasErrorFinish } = scanAssistantParts(m);

    if (hasErrorFinish) {
      flushBurst();
      out.push({ kind: "message", message: m, index: i });
      return;
    }

    if (hasTool) {
      // Tool-bearing message: walk parts in order. tool_call/tool_result/
      // thinking flow into the burst. A text part is the model narrating
      // between actions ("OK, the file exists, now let me edit it") — those
      // are valuable, the operator wants them. So a text part FLUSHES the
      // burst and renders as a StandaloneText row in place, then a new
      // burst can start with the tools that follow.
      if (burstFirstID === "") burstFirstID = m.ID;
      for (let pi = 0; pi < m.Parts.length; pi++) {
        const p = m.Parts[pi];
        if (p.type === "tool_call" || p.type === "tool_result" || p.type === "thinking") {
          burstParts.push(burstPart(m, p, pi));
        } else if (p.type === "text") {
          const t = p.Text ?? "";
          if (t.trim().length === 0) continue;
          flushBurst();
          out.push({ kind: "standalonetext", messageID: m.ID, partIndex: pi, text: t });
        }
      }
      return;
    }

    // Pure-prose / pure-thinking assistant message — the final answer
    // most often. Flush burst (positions the accordion before the answer)
    // and render the whole message.
    flushBurst();
    out.push({ kind: "message", message: m, index: i });
  });

  flushBurst();
  return out;
}
