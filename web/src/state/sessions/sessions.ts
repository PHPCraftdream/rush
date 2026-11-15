import { SessionsCarburetor } from "./SessionsCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const sessionsCarburetor = new SessionsCarburetor({ sessions: [], activeSessionID: null });
