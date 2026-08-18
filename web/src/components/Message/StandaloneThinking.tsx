// Wrapper rendering a ThinkingPart that was extracted out of its message.
// Pure code move from the former components/Message.tsx.

import { ThinkingPart } from "./ThinkingPart";

// StandaloneThinking is the renderer for a thinking part that was extracted
// out of its assistant message because the surrounding tool parts were
// folded into a cross-message ToolRun. It reuses ThinkingPart so the edit /
// delete / copy / sticky-collapse affordances stay identical to the in-
// message rendering. Wrapped in a small flex container so it sits in the
// chat scroll list at the same horizontal padding as a message row.
export function StandaloneThinking({ messageID, partIndex, thinking, done, model, effort }: { messageID: string; partIndex: number; thinking: string; done: boolean; model?: string; effort?: string }) {
  return (
    <div className="msg-row flex flex-col px-5 py-2">
      <div className="w-full min-w-0">
        <ThinkingPart messageID={messageID} partIndex={partIndex} thinking={thinking} done={done} model={model} effort={effort} />
      </div>
    </div>
  );
}
