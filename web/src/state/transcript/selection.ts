import { SelectionCarburetor } from "./SelectionCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const selectionCarburetor = new SelectionCarburetor({ selectedMessageIDs: [] });
