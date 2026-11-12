import type { TReadonly } from "@react-carburetor/core";
import type { ConfigPayload } from "../../types";

/** Name and context window of one configured model. */
export interface IModelInfo {
  name: string | null;
  contextWindow: number;
}

/**
 * Looks up a provider:::model key the way the model selector lists models: providers without
 * usable credentials are hidden (local CLI models are exempt).
 *
 * @param config - the server config
 * @param key - provider:::model key, or "" for none
 */
export function lookupModel(config: TReadonly<ConfigPayload> | null, key: string): IModelInfo {
  if (!key) return { name: null, contextWindow: 0 };
  for (const [providerID, p] of Object.entries(config?.providers ?? {})) {
    if (!(p.enabled ?? false) && (p.type ?? "") !== "cli") continue;
    for (const m of p.models ?? []) {
      if (`${providerID}:::${m.id}` === key) return { name: m.name || m.id, contextWindow: m.contextWindow ?? 0 };
    }
  }
  return { name: null, contextWindow: 0 };
}
