import { computed, shallowEqual } from "@react-carburetor/core";
import { getDefaultModelKey } from "../../../../actions";
import type { ConfigPayload } from "../../../../domain/types";
import { configCarburetor } from "../../../../state/config/config";
import { sessionsCarburetor } from "../../../../state/sessions/sessions";
import { findSession } from "../../../shared/findSession";
import { lookupModel } from "./lookupModel";

/**
 * The smart model the active session runs on: its own override, else the configured default.
 *
 * The cast hands the read-only tracked config to the key builder, which only reads it.
 */
export const activeSmartModel = computed(
  (read) => {
    const { sessions, activeSessionID } = read(sessionsCarburetor);
    const { config } = read(configCarburetor);
    const session = findSession(sessions, activeSessionID);
    const provider = session?.SmartModelProvider;
    const model = session?.SmartModelID;
    const key =
      provider && model ? `${provider}:::${model}` : getDefaultModelKey("smart", config as ConfigPayload | null);
    return lookupModel(config, key);
  },
  { equals: shallowEqual },
);
