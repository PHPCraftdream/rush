// Subscribes to the global "Collapse all" nonce and invokes the caller's
// collapse callback on every tick after mount.
// Pure code move from the former components/Message.tsx.

import { useRef, useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $collapseAllNonce } from "../../store";

// useCollapseAllSignal calls the provided collapser whenever the operator
// clicks "Collapse all" in the toolbar (i.e. the global $collapseAllNonce
// counter ticks). The first run after mount is intentionally skipped so a
// freshly-rendered spoiler with its default open-state isn't forced shut
// just because it subscribed.
export function useCollapseAllSignal(collapse: () => void) {
  const nonce = useStore($collapseAllNonce);
  const seen = useRef(nonce);
  useEffect(() => {
    if (seen.current === nonce) return;
    seen.current = nonce;
    collapse();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nonce]);
}
