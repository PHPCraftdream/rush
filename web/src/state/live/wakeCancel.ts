import { WakeCancelCarburetor } from "./WakeCancelCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const wakeCancelCarburetor = new WakeCancelCarburetor({ error: null });
