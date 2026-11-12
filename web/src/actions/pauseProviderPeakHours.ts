import { wsRequest } from "../net/wsRequest";
import type { ConfigScope } from "./ConfigScope";

/**
 * Temporarily suspends a provider peak-hours enforcement (any provider) until a backend-computed expiry.
 *
 * The expiry is a relative duration ("2h", "7d"), an absolute RFC3339 "until"
 * timestamp, or NO duration at all: the server then picks the end of the
 * currently active band. The server resolves and stores the resulting
 * pausedUntil; the browser never derives it. The returned promise rejects on
 * server errors; the caller renders them.
 *
 * @param payload - provider id, expiry and scope
 */
export async function pauseProviderPeakHours(payload: {
  id: string;
  duration?: string;
  until?: string;
  scope?: ConfigScope;
}) {
  const reply = await wsRequest<{ status: string }>("pause_provider_peak_hours", payload);
  return reply.payload;
}
