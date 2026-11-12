import { ConfigCarburetor } from "./ConfigCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const configCarburetor = new ConfigCarburetor({
  config: null,
  recent: { smart: [], fast: [], worker: [], reviewer: [] },
});
