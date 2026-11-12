import { Carburetor } from "@react-carburetor/core";

/**
 * Inline error from a rejected wake-schedule cancel (unknown/foreign
 * scheduleID, transport): shown inside the still-open ConfirmDialog, never
 * in the global banner.
 */
export interface IWakeCancelState {
  error: string | null;
}

/**
 * Writes the cancel error into the draft.
 *
 * @param draft - state to change
 * @param error - the error text, or null to clear
 */
const writeError = (draft: IWakeCancelState, error: string | null): void => {
  draft.error = error;
};

/** Holds the last rejected-cancel error for the Schedules tab dialog. */
export class WakeCancelCarburetor extends Carburetor<IWakeCancelState> {
  /**
   * Shows or clears the inline error.
   *
   * @param error - the error text, or null to clear
   */
  public setError(error: string | null): void {
    this.update(writeError, error);
  }
}
