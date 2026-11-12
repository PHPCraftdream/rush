import { Carburetor } from "@react-carburetor/core";

/**
 * Version pair announced by the server. Sent once per server start and only
 * when a newer release exists, so a non-null value always means "there is an
 * update".
 */
export interface IUpdateInfo {
  current: string;
  latest: string;
}

/** Transient banners: "update available" and the last agent error. */
export interface INoticesState {
  updateAvailable: IUpdateInfo | null;
  agentError: string | null;
}

/**
 * Writes the update announcement into the draft.
 *
 * @param draft - state to change
 * @param info - versions announced by the server
 */
const writeUpdateAvailable = (draft: INoticesState, info: IUpdateInfo | null): void => {
  draft.updateAvailable = info;
};

/**
 * Writes the agent error text into the draft.
 *
 * @param draft - state to change
 * @param message - the error text, or null to dismiss
 */
const writeAgentError = (draft: INoticesState, message: string | null): void => {
  draft.agentError = message;
};

/** Holds the banners shown over the chat: update notice and agent error. */
export class NoticesCarburetor extends Carburetor<INoticesState> {
  /**
   * Stores the update announcement.
   *
   * @param info - versions announced by the server
   */
  public setUpdateAvailable(info: IUpdateInfo | null): void {
    this.update(writeUpdateAvailable, info);
  }

  /**
   * Shows an agent error banner.
   *
   * @param message - the error text
   */
  public setAgentError(message: string): void {
    this.update(writeAgentError, message);
  }

  /** Dismisses the agent error banner. */
  public clearAgentError(): void {
    this.update(writeAgentError, null);
  }
}
