import { memo, useEffect, useMemo, useRef, useState } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";
import { Bot } from "lucide-react";
import { $subAgentMessages, $busySessions } from "../store";
import { ws } from "../ws";
import type { Message, ContentPart } from "../types";
import { SummaryMessage } from "./Message/SummaryMessage";
import { isTerminallyFinished } from "./Message/textParts";

const MD_REMARK = [remarkGfm, remarkBreaks];
const MD_REHYPE = [rehypeHighlight];

function extractTextFromParts(parts?: ContentPart[]): string {
  if (!parts) return "";
  return parts
    .filter((p) => p.type === "text")
    .map((p) => (p as { type: "text"; Text: string }).Text)
    .join("\n");
}

const SubAgentMessage = memo(function SubAgentMessage({ message }: { message: Message }) {
  const text = useMemo(() => extractTextFromParts(message.Parts), [message.Parts]);
  const toolCalls = useMemo(
    () => (message.Parts ?? []).filter((p) => p.type === "tool_call") as Array<{ type: "tool_call"; Name: string; Input: string; Finished: boolean }>,
    [message.Parts],
  );

  if (!text && toolCalls.length === 0) return null;

  return (
    <div className="py-1">
      {toolCalls.map((tc, i) => (
        <div key={i} className="flex items-center gap-1.5 text-xs text-text-subtle py-0.5">
          <span className="text-mauve font-semibold">{tc.Name}</span>
          {!tc.Finished && <span className="animate-pulse">running...</span>}
        </div>
      ))}
      {text && (
        <div className="md text-sm text-text-muted">
          <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
        </div>
      )}
    </div>
  );
});

export const SubAgentBlock = memo(function SubAgentBlock({
  messageID,
  toolCallID,
  prompt,
}: {
  messageID: string;
  toolCallID: string;
  prompt: string;
}) {
  // Backend keys the sub-agent session as `${messageID}$$${toolCallID}`
  // (see internal/session/session.go CreateAgentToolSessionID), and the WS
  // layer stores its messages + busy-state under that exact composite ID
  // (useWS.ts: registerSubAgentSession / upsertSubAgentMessage). We MUST look
  // up by the same key — using toolCallID alone silently misses every event,
  // which left the block permanently empty ("Starting agent..."). Fall back
  // to the bare toolCallID only when messageID is unavailable (defensive;
  // should not happen for a live sub-agent).
  const subSessionID = messageID ? `${messageID}$$${toolCallID}` : toolCallID;
  const allSubMessages = useStore($subAgentMessages);
  const busySessions = useStore($busySessions);
  const messages = allSubMessages.get(subSessionID) ?? [];
  const isRunning = busySessions.has(subSessionID);

  // "done" means this sub-agent's RUN is over — never true while it still
  // works. Each available signal is wrong alone, so AND them:
  //
  // - busySessions alone lags for sub-agents: nothing broadcasts
  //   agent_busy when the coordinator spawns one, so the client learns
  //   busy state only from the 5s list_sessions poll. !isRunning by
  //   itself would flash "done" during the first seconds of every run.
  // - Finish parts alone lie mid-run: every turn-STEP ends with a real
  //   non-Partial finish that stays in the store next to the next step's
  //   live message, and the 2s checkpoint ticker stamps Partial=true
  //   finishes on the streaming message. Only the LAST assistant
  //   message's terminal finish counts, via the same shared
  //   isTerminallyFinished the main renderer uses (Message.tsx).
  //
  // Every termination path (normal, error, loop-detected, canceled,
  // halted-by-tool-result) stamps a non-Partial finish on the final
  // assistant message, so done flips true once the coordinator releases
  // the session. A run hard-killed mid-stream intentionally stays
  // done=false — the block stays open instead of claiming success.
  const done = useMemo(() => {
    if (isRunning) return false;
    const lastAssistant = [...messages].reverse().find((m) => m.Role === "assistant");
    return lastAssistant ? isTerminallyFinished(lastAssistant.Parts ?? []) : false;
  }, [isRunning, messages]);

  const label = useMemo(() => {
    if (!prompt) return "";
    const maxLen = 80;
    const text = prompt.length > maxLen ? prompt.slice(0, maxLen) + "..." : prompt;
    return text;
  }, [prompt]);

  // Lazy-load sub-agent messages on first mount when nothing is in the
  // store yet (the WS handler only auto-loads sub-sessions created during
  // the live session — past runs surfaced after a reload start empty).
  // Latch on the session ID rather than a bare boolean: a boolean latch
  // cannot distinguish "already asked for THIS session" from "already
  // asked for SOME session", so a reused instance handed a different
  // subSessionID would never load. Latching on the value itself is
  // correct independently of how callers key us.
  const requested = useRef<string | null>(null);
  useEffect(() => {
    if (requested.current === subSessionID) return;
    if (messages.length > 0) return;
    requested.current = subSessionID;
    ws.send("load_messages", { sessionID: subSessionID });
  }, [subSessionID, messages.length]);

  // Open while the sub-agent is still working (mirrors prior `open={!done}`
  // behaviour); the user's manual toggle wins once they touch the chevron.
  const [override, setOverride] = useState<boolean | undefined>(undefined);
  const open = override ?? !done;
  const prevDone = useRef(done);
  useEffect(() => {
    if (done && !prevDone.current) setOverride(undefined);
    prevDone.current = done;
  }, [done]);
  const toggle = () => setOverride(!open);

  return (
    <div className="sub-agent-block my-2">
      <button
        type="button"
        onClick={toggle}
        aria-expanded={open}
        className="sub-agent-toggle w-full text-left bg-transparent border-0"
      >
        <Bot size={15} className={`shrink-0 ${isRunning ? "text-accent animate-pulse" : "text-text-subtle"}`} />
        <span className="font-semibold text-sm">Agent</span>
        {isRunning && <span className="text-xs text-text-subtle animate-pulse">running...</span>}
        {done && <span className="text-xs text-green font-medium">done</span>}
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
          {messages
            .filter((m) => m.Role === "assistant" && !m.Hidden)
            .map((m) =>
              m.IsSummaryMessage ? (
                <SummaryMessage key={m.ID} message={m} />
              ) : (
                <SubAgentMessage key={m.ID} message={m} />
              ),
            )}
        </div>
      )}
    </div>
  );
});
