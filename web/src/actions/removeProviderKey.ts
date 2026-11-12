import { ws } from "../net/client";

/**
 * Removes a provider API key.
 *
 * @param providerID - provider id
 */
export function removeProviderKey(providerID: string): void {
  ws.send("remove_provider_key", { providerID });
}
