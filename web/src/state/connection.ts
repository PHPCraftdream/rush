import { ConnectionCarburetor } from "./ConnectionCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const connectionCarburetor = new ConnectionCarburetor({
  connected: false,
  serverShuttingDown: false,
});
