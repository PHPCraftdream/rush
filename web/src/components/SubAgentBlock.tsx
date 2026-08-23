import { memo, useEffect, useMemo, useRef, useState } from "react";
import { useStore } from "@nanostores/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";
import { Bot } from "lucide-react";
import { $subAgentMessages, $messages, $activeSessionID, registerSubAgentSession } from "../store";
import { ws } from "../ws";
import type { Message, ContentPart, FinishPart } from "../types";
import { SummaryMessage } from "./Message/SummaryMessage";
import { FinishErrorBlock } from "./Message/FinishErrorBlock";
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
  finished,
}: {
  messageID: string;
  toolCallID: string;
  prompt: string;
  finished: boolean;
}) {
  // Backend keys the sub-agent session as `${messageID}$$${toolCallID}`
  // (see internal/session/session.go CreateAgentToolSessionID), and the WS
  // layer stores its messages under that exact composite ID
  // (useWS.ts: registerSubAgentSession / upsertSubAgentMessage). We MUST look
  // up by the same key — using toolCallID alone silently misses every event,
  // which left the block permanently empty ("Starting agent..."). Fall back
  // to the bare toolCallID only when messageID is unavailable (defensive;
  // should not happen for a live sub-agent).
  const subSessionID = messageID ? `${messageID}$$${toolCallID}` : toolCallID;
  const allSubMessages = useStore($subAgentMessages);
  // The parent message (the one whose agent tool_call part rendered this
  // block) lives in the active transcript whenever this block is mounted.
  // Look it up by ID: its SessionID is the owning session used to register
  // the lazy load below, and its finish state is the hard-kill fallback
  // for `done`.
  const parentMessages = useStore($messages);
  const parent = useMemo(
    () => parentMessages.find((m) => m.ID === messageID),
    [parentMessages, messageID],
  );
  const messages = allSubMessages.get(subSessionID) ?? [];

  // "done" means this sub-agent's RUN is over. Both signals are facts
  // about the PARENT message, not inferences from the sub-agent's own
  // transcript:
  //
  // - `finished` (primary): the parent's agent tool_call part's Finished
  //   flag — true exactly when the agent tool returned, which is the
  //   whole sub-agent run. It arrives via the parent's own
  //   message_updated and never flips at the sub-agent's internal step
  //   boundaries.
  // - parent terminally finished (fallback): a hard-killed run never
  //   gets its tool call marked finished (FinishToolCall only runs on
  //   the normal return path, internal/agent/agent_turn.go), but startup
  //   recovery stamps a terminal error finish on the parent's message
  //   (internal/app/app_recovery.go, "Process restarted"). Partial
  //   checkpoint finishes on a live parent don't count —
  //   isTerminallyFinished ignores them — so this stays false mid-run.
  //
  // isRunning is !done. A busySessions lookup cannot be used here: a
  // sub-agent session ID NEVER enters $busySessions — the only feeder
  // (the agent_busy event) names client-chosen top-level sessions or the
  // list_sessions correction loop, whose SQL (internal/db/sql/
  // sessions.sql, ListSessions) filters `parent_session_id is NULL`, so
  // sub-sessions are invisible to it. The previous predicate
  // (!isRunning && isTerminallyFinished(sub's last assistant message))
  // therefore flashed done at EVERY sub-agent step boundary (each step
  // ends with a real non-Partial finish while the run continues), which
  // also silently reset the operator's collapse override via the
  // prevDone effect below.
  const parentDone = parent ? isTerminallyFinished(parent.Parts ?? []) : false;
  const done = finished || parentDone;
  const isRunning = !done;

  // A run that ended badly must not wear the green "done" badge: read
  // the terminal finish of the sub-agent's last assistant message and
  // apply the same rule as the main renderer's finish router
  // (Message/Part.tsx): error or canceled renders an error block. When
  // the tool call never returned (`finished` false — a hard kill),
  // attribute the parent's terminal error finish instead; recovery
  // stamps "Process restarted" there and the sub-agent's own transcript
  // may have no finish at all.
  const errorFinish = useMemo(() => {
    const lastAssistant = [...messages].reverse().find((m) => m.Role === "assistant");
    if (lastAssistant) {
      const f = (lastAssistant.Parts ?? []).find(
        (p): p is FinishPart => p.type === "finish" && !p.Partial,
      );
      if (f && (f.Reason === "error" || f.Reason === "canceled")) return f;
    }
    if (!finished && parent) {
      const f = (parent.Parts ?? []).find(
        (p): p is FinishPart => p.type === "finish" && !p.Partial,
      );
      if (f && (f.Reason === "error" || f.Reason === "canceled")) return f;
    }
    return undefined;
  }, [messages, finished, parent]);

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
    // Register before asking: the messages_list router (useWS.ts) only
    // routes replies for sessions present in $subAgentSessions. After a
    // reload a historical sub-session is registered only if its
    // session_created survived the hub's replay ring — otherwise the
    // reply fell through to the main-chat branch and was silently
    // dropped, leaving this block empty forever. The owning session is
    // the parent message's own SessionID.
    const owner = parent?.SessionID ?? $activeSessionID.get();
    if (owner) registerSubAgentSession(subSessionID, owner);
    ws.send("load_messages", { sessionID: subSessionID });
  }, [subSessionID, messages.length, parent]);

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
        {done && (errorFinish ? (
          <span className="text-xs text-red font-medium">error</span>
        ) : (
          <span className="text-xs text-green font-medium">done</span>
        ))}
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
          {errorFinish && (
            <FinishErrorBlock reason={errorFinish.Reason} message={errorFinish.Message} details={errorFinish.Details} />
          )}
        </div>
      )}
    </div>
  );
});
