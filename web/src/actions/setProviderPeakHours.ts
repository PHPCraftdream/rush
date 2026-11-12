import { ws } from "../net/client";
import type { ProviderPeakHoursPolicy } from "../types";
import type { ConfigScope } from "./ConfigScope";

/**
 * Sets/clears ONLY the peak_hours field on ANY provider (built-in or custom).
 *
 * A targeted single-field write, unlike addCustomProvider/updateCustomProvider
 * which replace every field and are only safe on a custom provider the client
 * fully owns. It lets the UI manage peak hours for a built-in provider without
 * knowing or round-tripping its type, base URL, API key, or model list.
 *
 * @param payload - provider id, the policy (null clears it) and scope
 * @param msgID - correlation id for the reply
 */
export function setProviderPeakHours(
  payload: { id: string; peakHours: ProviderPeakHoursPolicy | null; scope?: ConfigScope },
  msgID?: string,
): void {
  ws.send("set_provider_peak_hours", payload, msgID);
}
