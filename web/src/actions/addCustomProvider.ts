import { ws } from "../net/client";
import type { ProviderPeakHoursPolicy } from "../types";
import type { ConfigScope } from "./ConfigScope";

/**
 * Adds a custom provider, replacing every field of an existing one with the same id.
 *
 * @param payload - the provider definition
 * @param msgID - correlation id for the reply
 */
export function addCustomProvider(
  payload: {
    id: string;
    name?: string;
    type: string;
    baseUrl: string;
    apiKey?: string;
    models?: { id: string; name: string; contextWindow?: number; costPer1mIn?: number; costPer1mOut?: number }[];
    peakHours?: ProviderPeakHoursPolicy | null;
    scope?: ConfigScope;
  },
  msgID?: string,
): void {
  ws.send("add_custom_provider", payload, msgID);
}
