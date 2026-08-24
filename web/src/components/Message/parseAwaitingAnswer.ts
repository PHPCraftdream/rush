// Parses the awaiting-answer finish text shape produced by the Go agent.
// Exported for reuse (e.g. tests).
// Pure code move from the former components/Message.tsx.

// ── ask_question rendering ────────────────────────────────────────────────
//
// The `ask_question` agent tool (internal/agent/tools/ask_question.go)
// force-finishes the turn instead of blocking mid-stream for an answer —
// this fork's web sessions have no separate blocking channel for that (see
// internal/agent/question_stop.go). The question ends up as an ordinary
// FinishReasonError part on the assistant message, and the "answer" is just
// the next normal chat message. This block is a pure convenience layer on
// top of that plain flow: it adds clickable option chips and an explicit
// free-text box, both of which funnel into the exact same `send_message`
// WS call ChatInput's `send()` uses.
//
// Detection caveat: there is currently NO structural signal for "this
// finish is a question, not a real error" on the wire — `rush run --json`
// computes exit_reason: "awaiting_answer" (internal/app/app.go
// buildRunResult), but that's CLI-only and never reaches the web socket.
// FinishPart only carries {Reason, Message, Details} and Reason is the
// generic "error" here, same as any other failure. So the only thing to
// key off is TEXT: the fixed title string awaitingAnswerStoppedFinishText
// always uses, plus the "QUESTION: " marker AwaitingAnswerGuidance always
// prefixes Details with. Both are internal/agent/question_stop.go string
// constants that this file has no compile-time link to — if that wording
// ever changes, parseAwaitingAnswer silently stops matching and the finish
// part just falls back to the plain FinishErrorBlock rendering (safe
// degradation, not a crash). The correct long-term fix is a dedicated
// structural field (e.g. Finish.Kind == "awaiting_answer") threaded through
// internal/server → the WS payload → FinishPart in types.ts; that's out of
// scope for this pass (pure frontend polish on top of the existing stream).
const AWAITING_ANSWER_TITLE = "Stopped: agent asked a question and is awaiting an answer";

interface ParsedQuestion {
  question: string;
  options: string[];
}

// parseAwaitingAnswer mirrors the exact string shape built by
// awaitingAnswerStoppedFinishText + AwaitingAnswerGuidance in
// internal/agent/question_stop.go:
//
//   Details = "<err.Error() line>\n\nQUESTION: <question>[\n\nSuggested options: a | b | c]\n\nThis is not a crash — …"
//
// Exported for reuse (e.g. tests); returns null for anything that doesn't
// match, which the caller treats as "not a question, render normally".
export function parseAwaitingAnswer(reason: string, msg: string, details: string): ParsedQuestion | null {
  if (reason !== "error" || msg !== AWAITING_ANSWER_TITLE) return null;
  const match = details.match(/QUESTION: ([\s\S]*?)\n\nThis is not a crash/);
  if (!match) return null;
  let body = match[1];
  const optMarker = "\n\nSuggested options: ";
  const optIdx = body.indexOf(optMarker);
  let options: string[] = [];
  if (optIdx !== -1) {
    options = body.slice(optIdx + optMarker.length).split(" | ").map(s => s.trim()).filter(Boolean);
    body = body.slice(0, optIdx);
  }
  const question = body.trim();
  if (!question) return null;
  return { question, options };
}
