import type { ModelItem } from "./modelListTypes";

/**
 * Models whose name, provider id, provider name or model id contains the query (already lower-cased).
 *
 * @param allModels - the catalog
 * @param q - the lower-cased query; empty matches nothing
 */
export function searchModels(allModels: readonly ModelItem[], q: string): ModelItem[] {
  if (!q) return [];
  return allModels.filter(
    (m) =>
      m.name.toLowerCase().includes(q) ||
      m.providerID.toLowerCase().includes(q) ||
      m.providerName.toLowerCase().includes(q) ||
      m.modelID.toLowerCase().includes(q),
  );
}
