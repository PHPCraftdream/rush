import { Carburetor } from "@react-carburetor/core";

/** WebSocket link and server-lifecycle flags of this browser tab. */
export interface IConnectionState {
  connected: boolean;
  serverShuttingDown: boolean;
}

/**
 * Writes the socket-connected flag into the draft.
 *
 * @param draft - state to change
 * @param connected - the new flag value
 */
const writeConnected = (draft: IConnectionState, connected: boolean): void => {
  draft.connected = connected;
};

/**
 * Marks the server as confirmed-shut-down in the draft.
 *
 * @param draft - state to change
 */
const writeShuttingDown = (draft: IConnectionState): void => {
  draft.serverShuttingDown = true;
};

/**
 * Tracks the socket link and the terminal "server is shutting down" state.
 *
 * The shutdown flag is terminal for the page: the server is intentionally
 * gone, so there is no way back to false.
 */
export class ConnectionCarburetor extends Carburetor<IConnectionState> {
  /**
   * Records whether the WebSocket is currently open.
   *
   * @param connected - the new flag value
   */
  public setConnected(connected: boolean): void {
    this.update(writeConnected, connected);
  }

  /** Records that the server confirmed our shutdown_server request. */
  public markServerShuttingDown(): void {
    this.update(writeShuttingDown);
  }
}
