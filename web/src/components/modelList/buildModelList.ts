import type { TReadonly } from "@react-carburetor/core";
import type { ConfigPayload } from "../../types";
import { buildProviderGroups } from "./buildProviderGroups";
import type { ModelItem, ProviderGroup } from "./modelListTypes";

/**
 * The models of a provider group.
 *
 * @param g - the group
 */
const groupModels = (g: ProviderGroup): ModelItem[] => g.models;

// Builds a flat list of all models from all providers

/**
 * Flattens every usable provider's models into one list.
 *
 * @param config - the server config
 */
export function buildModelList(config: TReadonly<ConfigPayload> | null): ModelItem[] {
  return buildProviderGroups(config).flatMap(groupModels);
}
