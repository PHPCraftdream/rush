import { Carburetor } from "@react-carburetor/core";

/** Authentication state of this browser session. */
export interface IAuthState {
  authed: boolean;
}

/**
 * Writes the authenticated flag into the draft.
 *
 * @param draft - state to change
 * @param authed - the new flag value
 */
const writeAuthed = (draft: IAuthState, authed: boolean): void => {
  draft.authed = authed;
};

/** Tracks whether the server accepted this browser session. */
export class AuthCarburetor extends Carburetor<IAuthState> {
  /**
   * Marks the session as authenticated or not.
   *
   * @param authed - the new flag value
   */
  public setAuthed(authed: boolean): void {
    this.update(writeAuthed, authed);
  }
}
