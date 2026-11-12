import { ws } from "../net/client";
import type { ConfigScope } from "./ConfigScope";

/**
 * Removes a custom provider.
 *
 * @param id - provider id
 * @param scope - config scope; defaults to global server-side
 * @param msgID - correlation id for the reply
 */
export function removeCustomProvider(id: string, scope?: ConfigScope, msgID?: string): void {
  ws.send("remove_custom_provider", { id, scope }, msgID);
}
