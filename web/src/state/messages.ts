import { mergePreserveContent } from "../mergeMessage";
import { MessagesCarburetor } from "./MessagesCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const messagesCarburetor = new MessagesCarburetor(
  { messages: [], subAgentSessions: {}, subAgentMessages: {}, messageBlockBreaks: {} },
  mergePreserveContent,
);
