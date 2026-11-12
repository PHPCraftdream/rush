// Subscribes to the global "jump to this tool call" request (task #1059) outside React.

import type { TReadonly } from "@react-carburetor/core";
import { signalsCarburetor } from "../../state/signals";
import type { ExpandToolCallRequest, ISignalsState } from "../../state/SignalsCarburetor";

/**
 * Picks the expand-tool-call request out of the signals state.
 *
 * @param state - the signals state
 */
const selectExpandRequest = (state: TReadonly<ISignalsState>) => state.expandToolCallRequest;

/**
 * Calls `onMatch` when the LiveWorkPanel dispatches a jump request for `toolCallID`.
 *
 * Unlike the collapse-all signal's skip-the-current-value pattern, the request
 * already pending at subscribe time IS delivered: a row that mounts fresh
 * because its enclosing ToolActivityGroup just expanded itself IN REACTION TO
 * THE SAME click still catches the request on its first effect, instead of
 * treating an already-pending nonce as "already seen". Call from an effect and
 * return the disposer as its cleanup.
 *
 * @param toolCallID - the tool call this subscriber represents
 * @param onMatch - invoked once per matching request
 * @returns the subscription disposer
 */
export const watchExpandToolCallSignal = (toolCallID: string | undefined, onMatch: () => void): (() => void) => {
  let seenNonce: number | undefined;
  const handle = (req: TReadonly<ExpandToolCallRequest> | null): void => {
    if (!req || seenNonce === req.nonce) return;
    seenNonce = req.nonce;
    if (toolCallID && req.toolCallID === toolCallID) onMatch();
  };
  handle(signalsCarburetor.getData().expandToolCallRequest);
  return signalsCarburetor.watch(selectExpandRequest, handle);
};
