import { Carburetor } from "@react-carburetor/core";
import type { ConfigPayload } from "../types";

/**
 * The four session model slots (task #1061 generalized ModelSelector from
 * smart/fast-only to all four).
 */
export type ModelRole = "smart" | "fast" | "worker" | "reviewer";

/** Server configuration and the recently used models per role. */
export interface IConfigState {
  config: ConfigPayload | null;
  /** Provider:::model keys, newest first; stored on the backend and synced via the config event. */
  recent: Record<ModelRole, string[]>;
}

/** How many recent models are kept per role. */
const RECENT_LIMIT = 5;

/**
 * Writes the config payload into the draft.
 *
 * @param draft - state to change
 * @param config - the payload the server sent
 */
const writeConfig = (draft: IConfigState, config: ConfigPayload | null): void => {
  draft.config = config;
};

/**
 * Writes one role's recent-model list into the draft.
 *
 * @param draft - state to change
 * @param role - model slot
 * @param keys - provider:::model keys, newest first
 */
const writeRecent = (draft: IConfigState, role: ModelRole, keys: string[]): void => {
  draft.recent[role] = keys;
};

/** Holds the server config and the per-role recent model lists. */
export class ConfigCarburetor extends Carburetor<IConfigState> {
  /**
   * Stores the config payload.
   *
   * @param config - the payload the server sent
   */
  public setConfig(config: ConfigPayload | null): void {
    this.update(writeConfig, config);
  }

  /**
   * Replaces one role's recent-model list.
   *
   * @param role - model slot
   * @param keys - provider:::model keys, newest first
   */
  public setRecent(role: ModelRole, keys: string[]): void {
    this.update(writeRecent, role, keys);
  }

  /**
   * Moves a model to the front of its role's recent list (capped at 5).
   *
   * @param role - model slot
   * @param modelKey - provider:::model key
   */
  public trackRecent(role: ModelRole, modelKey: string): void {
    const current = this.getData().recent[role];
    this.update(writeRecent, role, [modelKey, ...current.filter((k) => k !== modelKey)].slice(0, RECENT_LIMIT));
  }

  /**
   * Drops a model from its role's recent list.
   *
   * @param role - model slot
   * @param modelKey - provider:::model key
   */
  public removeRecent(role: ModelRole, modelKey: string): void {
    this.update(writeRecent, role, this.getData().recent[role].filter((k) => k !== modelKey));
  }
}
