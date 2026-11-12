import { computed, shallowEqual } from "@react-carburetor/core";
import { getDefaultModelKey } from "../../store";
import type { ConfigPayload } from "../../types";
import { configCarburetor } from "../../state/config";
import { sessionsCarburetor } from "../../state/sessions";
import { findSession } from "../Chat/findSession";
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
