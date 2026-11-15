import { selectionCarburetor } from "../../state/transcript/selection";

/** Empties the batch selection. */
export const clearSelection = (): void => {
  selectionCarburetor.clear();
};
