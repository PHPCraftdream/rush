import { ws } from "../net/client";

/**
 * Stores an API key for a provider.
 *
 * @param providerID - provider id
 * @param apiKey - the key
 */
export function setProviderKey(providerID: string, apiKey: string): void {
  ws.send("set_provider_key", { providerID, apiKey });
}
