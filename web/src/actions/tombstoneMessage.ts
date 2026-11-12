import { requestTracker } from "../net/requestTracker";
import { messagesCarburetor } from "../state/messages";

/**
 * Records that a message was deleted by a live push, so later merged snapshots keep it filtered out.
 *
 * The generation is the delete watermark carried on the push payload
 * (message_deleted DeleteGeneration, see internal/message/content.go). It is
 * forwarded to the request tracker so a LATER snapshot reply whose own
 * watermark is provably older than this delete is recognized as stale even when
 * the epoch heuristic alone would call it clean (tasks #731/#737). Undefined/0
 * (a missing/pre-watermark server) is a harmless no-op there: the tombstone
 * itself still protects this messageID via the existing epoch mechanism.
 *
 * @param sessionID - session the message belonged to
 * @param messageID - the deleted message
 * @param generation - delete watermark from the push
 */
export function tombstoneMessage(sessionID: string, messageID: string, generation?: number): void {
  messagesCarburetor.tombstone(sessionID, messageID);
  requestTracker.bumpDeleteHighWaterMark(sessionID, generation);
}
