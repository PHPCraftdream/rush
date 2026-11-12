// Completion index and prompt recall (R12-C); the pieces live in ./completion, one export per file.
export type { CompletionSource, CompletionRecord, MergedCompletionIndex, MyPromptItem } from "./completion/completionTypes";
export { structuredStatus } from "./completion/structuredStatus";
export { mergeCompletionIndex } from "./completion/mergeCompletionIndex";
export { completionStatuses } from "./completion/completionStatuses";
export { partsToText } from "./completion/partsToText";
export { isComposerTypedMessage } from "./completion/isComposerTypedMessage";
export { buildMyPrompts } from "./completion/buildMyPrompts";
