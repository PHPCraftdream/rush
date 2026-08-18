// Copies the agent's full turn (thinking + every text reply) for any message
// ID in the turn. Pure code move from the former components/Message.tsx.

import { useState, useCallback, memo } from "react";
import { Check, Copy } from "lucide-react";
import { collectTurnContent } from "../../store";

// CopyTurnButton copies the agent's full prose response to one user prompt —
// thinking + all intermediate text + final text, across every assistant
// message until the next user turn. Content is gathered LAZILY on click so a
// long streaming turn doesn't rebuild this string on every delta.
//
// Accepts ANY message ID belonging to the turn: a user prompt, an
// intermediate assistant step, or the final assistant message. collectTurnContent
// walks back to the turn's user message either way.
export const CopyTurnButton = memo(function CopyTurnButton({ messageID }: { messageID: string }) {
  const [copied, setCopied] = useState(false);
  const copy = useCallback(() => {
    const text = collectTurnContent(messageID);
    if (!text) return;
    navigator.clipboard.writeText(text).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); });
  }, [messageID]);
  return (
    <button onClick={copy} title="Copy all (thinking + every text reply, no tools)" className="btn-copy">
      {copied ? <><Check size={14} className="text-green" /><span className="text-green">Copied</span></> : <><Copy size={14} /><span>Copy all</span></>}
    </button>
  );
});
