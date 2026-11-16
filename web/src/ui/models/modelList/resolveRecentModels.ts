import type { ModelItem } from "./modelListTypes";

/**
 * Whether a lookup hit.
 *
 * @param m - the lookup result
 */
const isFound = (m: ModelItem | undefined): m is ModelItem => !!m;

/**
 * Resolves recent model keys to their catalog entries, dropping unknown keys.
 *
 * @param recentKeys - provider:::model keys, newest first
 * @param allModels - the catalog
 */
export function resolveRecentModels(recentKeys: readonly string[], allModels: readonly ModelItem[]): ModelItem[] {
  return recentKeys.map((k) => allModels.find((m) => m.key === k)).filter(isFound);
}
