import type { TReadonly } from "@react-carburetor/core";
import type { ConfigPayload } from "../../types";
import type { ModelItem, ProviderGroup } from "./modelListTypes";

/**
 * Orders groups by provider name.
 *
 * @param a - first group
 * @param b - second group
 */
const byName = (a: ProviderGroup, b: ProviderGroup): number => a.name.localeCompare(b.name);

// Builds a list of provider groups, each with their models. Every ModelItem is
// plain data (its reasoning ladder is copied), so it can outlive the render
// that read the config.

/**
 * Builds the provider groups of the usable providers, sorted by name.
 *
 * @param config - the server config
 */
export function buildProviderGroups(config: TReadonly<ConfigPayload> | null): ProviderGroup[] {
  const groups: ProviderGroup[] = [];
  const seen = new Set<string>();
  for (const [providerID, p] of Object.entries(config?.providers ?? {})) {
    const enabled = p.enabled ?? false;
    const providerName = p.name || providerID;
    const providerType = p.type ?? "";
    const models: ModelItem[] = [];
    for (const m of p.models ?? []) {
      const key = `${providerID}:::${m.id}`;
      if (!seen.has(key)) {
        seen.add(key);
        models.push({
          key,
          providerID,
          providerName,
          providerType,
          modelID: m.id,
          name: m.name || m.id,
          contextWindow: m.contextWindow ?? 0,
          enabled,
          reasoningLevels: m.reasoningLevels ? [...m.reasoningLevels] : undefined,
          defaultReasoningEffort: m.defaultReasoningEffort,
        });
      }
    }
    // Hide providers without usable credentials; local CLI models are exempt.
    if (models.length > 0 && (enabled || providerType === "cli")) {
      groups.push({ id: providerID, name: providerName, type: providerType, enabled, models });
    }
  }
  groups.sort(byName);
  return groups;
}
