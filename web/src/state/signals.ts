import { SignalsCarburetor } from "./SignalsCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const signalsCarburetor = new SignalsCarburetor({ collapseAllNonce: 0, expandToolCallRequest: null });
