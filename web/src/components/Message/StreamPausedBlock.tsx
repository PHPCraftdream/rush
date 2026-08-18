// Soft amber notice for watchdog-stalled streams with substantive work done.
// Pure code move from the former components/Message.tsx.

import { memo } from "react";

// StreamPausedBlock — soft notice for a watchdog stall that happened AFTER
// the model already produced substantive output. The work above is intact;
// only the tail of the stream was cut. Distinct from FinishErrorBlock to
// stop the UI from screaming "ERROR" when nothing in the turn actually
// failed — the user can re-prompt to continue with the inventory.
export const StreamPausedBlock = memo(function StreamPausedBlock({ details }: { details: string }) {
  return (
    <div data-test-id="stream-paused" className="tool-block my-2 border-yellow/40 bg-yellow/[6%]">
      <div className="flex items-center gap-2 mb-1">
        <span className="text-yellow font-semibold text-sm">Stream paused</span>
        <span className="text-text-subtle text-xs">watchdog cut the tail · work above is intact</span>
      </div>
      {details && <pre className="tool-output whitespace-pre-wrap text-text-subtle">{details}</pre>}
    </div>
  );
});
