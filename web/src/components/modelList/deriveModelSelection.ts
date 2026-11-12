import type { TReadonly } from "@react-carburetor/core";
import type { ModelRole } from "../../state/ConfigCarburetor";
import type { ConfigPayload, Session } from "../../types";
import { effortLevelsFor, defaultEffortFor, supportsEffort, clampEffort } from "../../effort";
import { defaultModelKey } from "./defaultModelKey";
import { buildProviderGroups } from "./buildProviderGroups";
import type { ModelItem, ProviderGroup } from "./modelListTypes";

/** Everything a role's model selector shows, derived from the config and the session. */
export interface IModelSelection {
  allModels: ModelItem[];
  providerGroups: ProviderGroup[];
  currentKey: string;
  hasSessionOverride: boolean;
  displayName: string;
  effortLevels: readonly string[];
  showEffortPicker: boolean;
  effortValid: boolean;
  currentEffort: string;
}

/**
 * The models of a provider group.
 *
 * @param g - the group
 */
const groupModels = (g: ProviderGroup): ModelItem[] => g.models;

/**
 * Derives the selector's view of one role.
 *
 * Session field names for the role, e.g. "worker" -> WorkerModelProvider /
 * WorkerModelID / WorkerModelReasoningEffort — same computed-key convention
 * ScopedModelsModal.tsx's SessionSlotRow uses, so the two surfaces can't drift
 * on how a role name maps to its Session columns.
 *
 * @param config - the server config
 * @param session - the active session, if any
 * @param modelType - the role
 */
export function deriveModelSelection(
  config: TReadonly<ConfigPayload> | null,
  session: Session | null,
  modelType: ModelRole,
): IModelSelection {
  const providerGroups = buildProviderGroups(config);
  const allModels = providerGroups.flatMap(groupModels);
  const defaultKey = defaultModelKey(modelType, config);

  const rolePrefix = `${modelType[0].toUpperCase()}${modelType.slice(1)}`;
  const providerField = `${rolePrefix}ModelProvider` as keyof Session;
  const idField = `${rolePrefix}ModelID` as keyof Session;
  const effortField = `${rolePrefix}ModelReasoningEffort` as keyof Session;

  // Get current key from session record if available, else use global default
  const sessionProvider = session?.[providerField] as string | undefined;
  const sessionModelID = session?.[idField] as string | undefined;
  const hasSessionOverride = !!(sessionProvider && sessionModelID);
  let currentKey = defaultKey;
  if (hasSessionOverride) {
    currentKey = `${sessionProvider}:::${sessionModelID}`;
  }

  const currentEntry = allModels.find((m) => m.key === currentKey);
  const displayName = currentEntry?.name ?? currentKey.split(":::")[1] ?? "No model";

  const currentProvider = currentEntry?.providerID ?? "";
  const currentModelID = currentEntry?.modelID ?? "";
  const effortLevels = effortLevelsFor(currentProvider, currentModelID, currentEntry) ?? [];
  const explicitEffort = session?.[effortField] as string | undefined;
  const storedEffort = explicitEffort || defaultEffortFor(currentProvider, currentModelID, currentEntry);
  const showEffortPicker = supportsEffort(currentProvider, currentModelID, currentEntry);
  const clampedEffort = clampEffort(currentProvider, currentModelID, storedEffort, currentEntry);
  const effortValid = !explicitEffort || clampedEffort === explicitEffort;
  const currentEffort = storedEffort ? (clampedEffort ?? storedEffort) : "";

  return {
    allModels,
    providerGroups,
    currentKey,
    hasSessionOverride,
    displayName,
    effortLevels,
    showEffortPicker,
    effortValid,
    currentEffort,
  };
}
