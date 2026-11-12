import { wsRequest } from "../net/wsRequest";
import type { ConfigScope } from "./ConfigScope";

/**
 * Clears an earlier pause, restoring immediate enforcement of the configured peak-hours policy.
 *
 * The returned promise rejects on server errors; the caller renders them.
 *
 * @param payload - provider id and scope
 */
export async function resumeProviderPeakHours(payload: { id: string; scope?: ConfigScope }) {
  const reply = await wsRequest<{ status: string }>("resume_provider_peak_hours", payload);
  return reply.payload;
}
