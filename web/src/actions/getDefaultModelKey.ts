import type { ModelRole } from "../state/ConfigCarburetor";
import type { ConfigPayload } from "../types";

/**
 * The provider:::model key of the configured default for a role, or "" when none.
 *
 * Per-session model overrides were removed in favor of global selection; the
 * session object from the DB is the source of truth.
 *
 * @param role - model slot
 * @param config - the server config
 */
export function getDefaultModelKey(role: ModelRole, config: ConfigPayload | null): string {
  const entry = config?.models?.[role];
  if (entry) return `${entry.Provider}:::${entry.Model}`;
  return "";
}
