import type { ModelRole } from "../state/ConfigCarburetor";

/**
 * Maps a ModelRole to its Session field prefix (Smart/Fast/Worker/Reviewer).
 *
 * Lets the per-role setters read/write `${prefix}ModelProvider` etc. without a
 * role-keyed literal object at every call site.
 *
 * @param role - model slot
 */
export function roleFieldPrefix(role: ModelRole): string {
  return `${role[0].toUpperCase()}${role.slice(1)}`;
}
