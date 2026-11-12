import { selectionCarburetor } from "../../state/selection";

/** Empties the batch selection. */
export const clearSelection = (): void => {
  selectionCarburetor.clear();
};
