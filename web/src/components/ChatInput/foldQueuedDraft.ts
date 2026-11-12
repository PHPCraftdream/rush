import type { QueuedMessage, WireAttachment } from "../../store";
import type { PendingAttachment } from "./PendingAttachment";
import { toWireAttachments } from "./toWireAttachments";

/** The draft folded together with the locally queued messages. */
export interface IFoldedDraft {
  content: string;
  atts: WireAttachment[];
  /** Whether any locally queued message was folded in (and so must be dequeued once sent). */
  hadQueued: boolean;
}

/**
 * Text of a queued message.
 *
 * @param message - the queued message
 */
const contentOf = (message: QueuedMessage): string => message.content;

/**
 * Attachments of a queued message, or none.
 *
 * @param message - the queued message
 */
const attachmentsOf = (message: QueuedMessage): WireAttachment[] => message.attachments ?? [];

/**
 * Folds anything sitting in the local front-end queue into the submitted text
 * (both text AND attachments), otherwise those would race the interrupt/inject
 * and arrive in a separate send_message afterwards.
 *
 * @param queuedMessages - the active session's local queue
 * @param text - the composer draft
 * @param attachments - the composer's pending attachments
 */
export function foldQueuedDraft(
  queuedMessages: readonly QueuedMessage[],
  text: string,
  attachments: readonly PendingAttachment[],
): IFoldedDraft {
  const hadQueued = queuedMessages.length > 0;
  const parts: string[] = [];
  const atts: WireAttachment[] = [];
  if (hadQueued) {
    parts.push(queuedMessages.map(contentOf).join("\n\n"));
    atts.push(...queuedMessages.flatMap(attachmentsOf));
  }
  const msg = text.trim();
  if (msg) parts.push(msg);
  atts.push(...toWireAttachments(attachments));
  return { content: parts.join("\n\n"), atts, hadQueued };
}
