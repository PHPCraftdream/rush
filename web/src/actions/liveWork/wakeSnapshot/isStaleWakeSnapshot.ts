/**
 * True only for a tagged response superseded by a later request for the same session.
 *
 * Untagged pushes and replies from older clients remain accepted.
 *
 * @param latestBySession - newest request id per session
 * @param sessionID - session of the snapshot
 * @param requestID - correlation id of the reply
 * @param isTaggedRequest - whether an id belongs to a tracked request kind
 */
export function isStaleWakeSnapshot(
  latestBySession: ReadonlyMap<string, string>,
  sessionID: string | undefined,
  requestID: string | undefined,
  isTaggedRequest: (id: string) => boolean,
): boolean {
  if (!sessionID || !requestID || !isTaggedRequest(requestID)) return false;
  const latest = latestBySession.get(sessionID);
  return !!latest && latest !== requestID;
}
