// Interactive block for an agent that stopped to ask the operator a question.
// Pure code move from the former components/Message.tsx.

import { useState, useCallback, memo } from "react";
import { HelpCircle, X } from "lucide-react";
import { ws } from "../../ws";

// AskQuestionBlock — the agent is waiting for an answer. Renders the
// question as plain text (matches how a normal reply reads), a row of
// clickable chips for any suggested options, and an ALWAYS-VISIBLE inline
// free-text input + send button (explicitly requested: don't rely on the
// operator noticing they can just type in the chat box below). Both paths
// send through the normal chat pipeline — no new backend state, no special
// "answered" flag. "Dismiss" only hides this block locally; it sends
// nothing and does not mark anything answered server-side.
export const AskQuestionBlock = memo(function AskQuestionBlock({ question, options, sessionID }: { question: string; options: string[]; sessionID: string }) {
  const [dismissed, setDismissed] = useState(false);
  const [answer, setAnswer] = useState("");

  const answerWith = useCallback((text: string) => {
    const trimmed = text.trim();
    if (!trimmed || !sessionID) return;
    ws.send("send_message", { sessionID, content: trimmed });
    setDismissed(true);
  }, [sessionID]);

  const onChipClick = useCallback((opt: string) => answerWith(opt), [answerWith]);

  const onSubmit = useCallback((e: React.FormEvent) => {
    e.preventDefault();
    answerWith(answer);
  }, [answer, answerWith]);

  const onAnswerChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => setAnswer(e.target.value), []);
  const onDismiss = useCallback(() => setDismissed(true), []);

  if (dismissed) return null;

  return (
    <div data-test-id="ask-question-block" className="tool-block my-2 border-accent/40 bg-accent/[6%]">
      <div className="flex items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2">
          <HelpCircle size={15} className="text-accent shrink-0" />
          <span className="text-accent font-semibold text-sm">Agent is waiting for your answer</span>
        </div>
        <button
          type="button"
          onClick={onDismiss}
          title="Dismiss — does not send anything"
          className="text-text-subtle hover:text-text transition-colors shrink-0"
        >
          <X size={15} />
        </button>
      </div>

      <p className="text-text text-sm leading-relaxed mb-3 whitespace-pre-wrap">{question}</p>

      {options.length > 0 && (
        <div data-test-id="ask-question-options" className="flex flex-wrap gap-2 mb-3">
          {options.map((opt, i) => (
            <button
              key={i}
              type="button"
              onClick={() => onChipClick(opt)}
              className="px-3 py-1.5 text-sm rounded-full border border-accent/40 text-accent hover:bg-accent/15 active:scale-[0.97] transition-all"
            >
              {opt}
            </button>
          ))}
        </div>
      )}

      <form onSubmit={onSubmit} className="flex items-center gap-2">
        <input
          type="text"
          value={answer}
          onChange={onAnswerChange}
          placeholder="Type your answer…"
          data-test-id="ask-question-answer-input"
          className="flex-1 bg-base-subtle border border-surface rounded-lg px-3 py-2 text-sm text-text outline-none focus:border-accent"
        />
        <button
          type="submit"
          disabled={!answer.trim()}
          data-test-id="ask-question-answer-submit"
          className="px-3 py-2 text-sm btn-primary disabled:opacity-30"
        >
          Send
        </button>
      </form>
    </div>
  );
});
