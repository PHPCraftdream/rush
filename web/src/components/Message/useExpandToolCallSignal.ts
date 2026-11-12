// Subscribes to the global "jump to this tool call" request (task #1059)
// and invokes the caller's onMatch when the request targets its own id.

import { useRef, useEffect } from "react";
import type { TReadonly } from "@react-carburetor/core";
import { useCarburetorValue } from "@react-carburetor/core/interop";
import { signalsCarburetor } from "../../state/signals";
import type { ISignalsState } from "../../state/SignalsCarburetor";

/** Picks the expand-tool-call request out of the signals state. */
const selectExpandRequest = (state: TReadonly<ISignalsState>) => state.expandToolCallRequest;

// useExpandToolCallSignal calls `onMatch` when the LiveWorkPanel dispatches
// a jump request for `toolCallID`. Unlike useCollapseAllSignal's plain
// skip-first-mount pattern, the seen-nonce ref starts at `undefined` --not
// the request's CURRENT nonce-- so a row that mounts fresh because its
// enclosing ToolActivityGroup just expanded itself IN REACTION TO THE SAME
// click still catches the request on this, its first render, instead of
// treating an already-pending nonce as "already seen".
export function useExpandToolCallSignal(toolCallID: string | undefined, onMatch: () => void) {
  const req = useCarburetorValue(signalsCarburetor, selectExpandRequest);
  const seenNonce = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (!req || seenNonce.current === req.nonce) return;
    seenNonce.current = req.nonce;
    if (toolCallID && req.toolCallID === toolCallID) onMatch();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [req, toolCallID]);
}
