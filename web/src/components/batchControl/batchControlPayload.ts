import type { BatchControlTarget } from "./batchControlTypes";

/**
 * Builds the wire payload addressing a batch target.
 *
 * @param target - the addressed session, root and node
 */
export function batchControlPayload(target: BatchControlTarget) {
  return {
    sessionID: target.sessionID,
    rootID: target.rootID ?? "",
    nodeID: target.nodeID ?? "",
    ...(target.waitMS === undefined ? {} : { waitMS: target.waitMS }),
  };
}
