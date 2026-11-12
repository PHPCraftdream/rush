import { ws } from "../net/client";

/**
 * Toggles the WebAudio keep-alive preference.
 *
 * The backend persists it to the global rush.json under
 * options.keep_alive_enabled and broadcasts a fresh `config` event; connectWS
 * reacts to that broadcast and starts/stops the local audio.
 *
 * @param enabled - the new preference
 */
export function setKeepAliveEnabled(enabled: boolean): void {
  ws.send("set_keep_alive", { enabled });
}
