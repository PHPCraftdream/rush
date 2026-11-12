import { ws } from "../net/client";
import type { ProviderPeakHoursPolicy } from "../types";
import type { ConfigScope } from "./ConfigScope";

/**
 * Replaces every field of a custom provider (renaming it when id differs from oldId).
 *
 * @param payload - the provider definition plus its previous id
 * @param msgID - correlation id for the reply
 */
export function updateCustomProvider(
  payload: {
    oldId: string;
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
  ws.send("update_custom_provider", payload, msgID);
}
