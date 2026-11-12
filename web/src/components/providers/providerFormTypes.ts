import type { ConfigScope } from "../../store";
import type { ProviderPeakHoursPolicy } from "../../types";
import type { ModelDraft } from "./ModelDraft";

/** Values a provider form starts from when editing an existing provider. */
export interface IProviderFormInitial {
  id: string;
  name: string;
  type: string;
  baseUrl: string;
  models: ModelDraft[];
  peakHours?: ProviderPeakHoursPolicy | null;
  scope?: ConfigScope;
}

/** Values a provider form submits. */
export interface IProviderFormData {
  id: string;
  name: string;
  type: string;
  baseUrl: string;
  apiKey: string;
  models: ModelDraft[];
  peakHours: ProviderPeakHoursPolicy | null;
  scope: ConfigScope;
}
