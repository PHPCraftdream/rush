import { computed } from "@react-carburetor/core";
import { isAsyncCompletionNotice } from "../asyncJob/isAsyncCompletionNotice";
import { buildMyPrompts } from "../completion/buildMyPrompts";
import type { Message } from "../types";
import { messagesCarburetor } from "./messages";

/**
 * The composer's prompt history, derived from the active transcript.
 *
 * Feeds shell-style recall (ArrowUp/Down) and the history dropdown. Eligibility
 * is decided by isComposerTypedMessage / buildMyPrompts. The cast hands the
 * read-only tracked transcript to a builder that only reads it.
 */
export const myPrompts = computed((read) =>
  buildMyPrompts(read(messagesCarburetor).messages as readonly Message[], isAsyncCompletionNotice),
);
