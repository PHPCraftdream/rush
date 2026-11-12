import { wsRequest } from "../net/wsRequest";
import type { ProviderPeakHoursStatus } from "../types";

/**
 * Asks the backend for its authoritative peak-hours evaluation (get_peak_hours_status).
 *
 * Whether each provider is currently in peak/paused/blocked and when the next
 * transition happens. The browser must never compute this itself: all state and
 * timestamps come from the server; the UI only formats them.
 *
 * @param id - provider id; omitted means every provider
 */
export async function fetchPeakHoursStatus(
  id?: string,
): Promise<{ checkedAt: string; statuses: ProviderPeakHoursStatus[] }> {
  const reply = await wsRequest<{ checkedAt: string; statuses: ProviderPeakHoursStatus[] }>("get_peak_hours_status", {
    id,
  });
  return reply.payload ?? { checkedAt: "", statuses: [] };
}
