// oxlint-disable carburetor/one-export-per-file -- aggregate state/helpers module (sprint0-split-web §2.4)
import type { KeyboardEvent } from "react";
import type { TReadonly } from "@react-carburetor/core";
import type { QueuedMessage, WireAttachment } from "../../../../actions";
import { isAttachmentPayloadWithinLimit } from "../../../../domain/attachmentLimit";
import type { PendingAttachment } from "../attachments/PendingAttachment";
import type { ISkillsState } from "../../../../state/config/SkillsCarburetor";
import { sessionsCarburetor } from "../../../../state/sessions/sessions";
/** Shown when the draft plus attachments would not fit one frame. */
export const PAYLOAD_BUDGET_ERROR =
  "Message exceeds the 19 MiB payload budget. Shorten the text or remove attachments before sending.";

/** Shown when the queued messages plus the draft would not fit one frame. */
export const QUEUE_BUDGET_ERROR =
  "Queued messages exceed the 19 MiB payload budget. Shorten the text or remove attachments before sending.";

/** Shown when a picked file alone exceeds the frame budget. */
export const FILE_BUDGET_ERROR = "A file exceeds the 19 MiB payload limit. Choose a smaller file.";

/** Local state of the composer. */
export interface IChatInputState {
  text: string;
  attachments: PendingAttachment[];
  attachmentError: string;
  /** -1 = live draft, 0..N-1 = a prompt from myPrompts (newest = 0). */
  histIdx: number;
  /**
   * Text the user was typing when they started recalling, so returning to
   * histIdx = -1 (or pressing Esc) restores it.
   */
  stash: string;
  /**
   * Whether the history dropdown is visible. Independent of histIdx:
   * shell-style ArrowUp/Down can recall without opening the menu.
   */
  historyOpen: boolean;
  slashOpen: boolean;
  slashIdx: number;
  /**
   * The composer only claims "offline" once a connection has been seen —
   * otherwise every fresh page load would flash the banner while the very
   * first socket is still connecting.
   */
  everConnected: boolean;
}

/**
 * Picks the skills list out of the skills state.
 *
 * @param state - the skills state
 */
export const selectSkills = (state: TReadonly<ISkillsState>) => state.skills;

/**
 * Whether a keydown carries no modifier key.
 *
 * @param e - the keydown event
 */
export const isBareKey = (e: KeyboardEvent<HTMLTextAreaElement>): boolean =>
  !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey;

/**
 * Text of a queued message.
 *
 * @param message - the queued message
 */
export const contentOf = (message: QueuedMessage): string => message.content;

/**
 * Attachments of a queued message, or none.
 *
 * @param message - the queued message
 */
export const attachmentsOf = (message: QueuedMessage): WireAttachment[] => message.attachments ?? [];

/**
 * Size record of a picked file.
 *
 * @param file - the file
 */
export const sizeOf = (file: File): { size: number } => ({ size: file.size });

/**
 * Whether a picked file alone exceeds the frame budget.
 *
 * @param file - the file's size record
 * @param file.size - bytes
 */
export const exceedsBudget = (file: { size: number }): boolean => !isAttachmentPayloadWithinLimit("", [file]);

/**
 * Flips the history dropdown.
 *
 * @param s - the composer state
 */
export const flipHistoryOpen = (s: IChatInputState): Pick<IChatInputState, "historyOpen"> => ({
  historyOpen: !s.historyOpen,
});

/**
 * Moves the history selection towards newer prompts, stopping at the newest.
 *
 * @param s - the composer state
 */
export const stepHistoryNewer = (s: IChatInputState): Pick<IChatInputState, "histIdx"> => ({
  histIdx: Math.max(s.histIdx - 1, 0),
});

/**
 * Moves the slash selection up, stopping at the first row.
 *
 * @param s - the composer state
 */
export const stepSlashUp = (s: IChatInputState): Pick<IChatInputState, "slashIdx"> => ({
  slashIdx: Math.max(s.slashIdx - 1, 0),
});

/**
 * The active session id, read from the store at call time.
 */
export const currentSessionID = (): string | null => sessionsCarburetor.getData().activeSessionID;
