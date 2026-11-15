import { Carburetor } from "@react-carburetor/core";

/** Messages ticked for batch actions. */
export interface ISelectionState {
  /** Unique ids in selection order; converted to a Set at the read edge. */
  selectedMessageIDs: string[];
}

/**
 * Replaces the selected set in the draft.
 *
 * @param draft - state to change
 * @param next - the new set
 */
const writeSelected = (draft: ISelectionState, next: string[]): void => {
  draft.selectedMessageIDs = next;
};

/** Holds the batch selection of messages. */
export class SelectionCarburetor extends Carburetor<ISelectionState> {
  /**
   * Flips one message in or out of the selection.
   *
   * @param id - message id
   */
  public toggle(id: string): void {
    const s = new Set(this.getData().selectedMessageIDs);
    if (s.has(id)) s.delete(id);
    else s.add(id);
    this.update(writeSelected, [...s]);
  }

  /** Empties the selection. */
  public clear(): void {
    this.update(writeSelected, []);
  }

  /**
   * Adds all given ids (only adds, never removes).
   *
   * @param ids - message ids
   */
  public selectAll(ids: readonly string[]): void {
    const s = new Set(this.getData().selectedMessageIDs);
    for (const id of ids) s.add(id);
    this.update(writeSelected, [...s]);
  }
}
