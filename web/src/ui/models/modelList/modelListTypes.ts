/** One selectable model of a configured provider. */
export interface ModelItem {
  key: string;
  providerID: string;
  providerName: string;
  providerType: string;
  modelID: string;
  name: string;
  contextWindow: number;
  reasoningLevels?: string[];
  defaultReasoningEffort?: string;
  enabled: boolean; // provider has usable credentials or is a local CLI
}

/** A provider with its selectable models. */
export interface ProviderGroup {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  models: ModelItem[];
}
