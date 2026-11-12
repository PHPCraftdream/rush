import { BusyCarburetor } from "./BusyCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const busyCarburetor = new BusyCarburetor({ busySessions: [], summarizeQueued: [] });
