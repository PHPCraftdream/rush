import type { TReadonly } from "@react-carburetor/core";
import type { ModelRole } from "../../../state/config/ConfigCarburetor";
import type { ConfigPayload } from "../../../domain/types";

/**
 * The provider:::model key of the configured default for a role, or "" when none.
 *
 * @param role - model slot
 * @param config - the server config
 */
export function defaultModelKey(role: ModelRole, config: TReadonly<ConfigPayload> | null): string {
  const entry = config?.models?.[role];
  if (entry) return `${entry.Provider}:::${entry.Model}`;
  return "";
}
