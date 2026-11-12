// Visible error/canceled finish block for failed turns.

import { memo } from "react";

/** Props of the finish error block. */
interface IFinishErrorBlockProps {
  reason: string;
  message: string;
  details: string;
}

/**
 * Fork patch: visible block for error / canceled finish parts (replaces the
 * previous "render nothing" behaviour that produced blank assistant bubbles).
 */
export const FinishErrorBlock = memo(function FinishErrorBlock({ reason, message, details }: IFinishErrorBlockProps) {
  const title = message || (reason === "canceled" ? "Canceled" : "Error");
  return (
    <div data-test-id="finish-error" className="tool-block my-2 border-red/40 bg-red/[6%]">
      <div className="flex items-center gap-2 mb-1">
        <span className="text-red font-semibold text-sm">{title}</span>
        <span className="badge-error">{reason}</span>
      </div>
      {details && <pre className="tool-output whitespace-pre-wrap">{details}</pre>}
    </div>
  );
});
