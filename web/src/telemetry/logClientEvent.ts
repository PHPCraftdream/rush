import { ws } from "../net/client";
import { configCarburetor } from "../state/config";

/**
 * Sends a user event to the backend when debug mode is on.
 *
 * @param event - event name
 * @param details - event details
 */
export function logClientEvent(event: string, details?: Record<string, unknown>): void {
  const cfg = configCarburetor.getData().config;
  if (!cfg?.debug) return;
  ws.send("log_client_event", { event, details });
}
