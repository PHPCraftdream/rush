import { computed } from "@react-carburetor/core";
import { completionStatuses } from "../domain/completion/completionStatuses";
import { asyncJobCompletions } from "./asyncJobCompletions";

/** Per-tool-call status map SubAgentBlock consumes, derived from the completion index. */
export const asyncJobStatuses = computed((read) => completionStatuses(read(asyncJobCompletions)));
