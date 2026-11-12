// Subscribes to the global "Collapse all" counter outside React.

import type { TReadonly } from "@react-carburetor/core";
import { signalsCarburetor } from "../../state/signals";
import type { ISignalsState } from "../../state/SignalsCarburetor";

/**
 * Picks the collapse-all counter out of the signals state.
 *
 * @param state - the signals state
 */
const selectCollapseAllNonce = (state: TReadonly<ISignalsState>): number => state.collapseAllNonce;

/**
 * Calls `collapse` whenever the operator clicks "Collapse all" in the toolbar.
 *
 * The global counter ticks on each click. Only ticks after this call count, so a
 * freshly-rendered spoiler with its default open-state isn't forced shut just
 * because it subscribed. Call from an effect and return the disposer as its cleanup.
 *
 * @param collapse - the collapser to invoke on each tick
 * @returns the subscription disposer
 */
export const watchCollapseAllSignal = (collapse: () => void): (() => void) =>
  signalsCarburetor.watch(selectCollapseAllNonce, collapse);
