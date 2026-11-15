import { NoticesCarburetor } from "./NoticesCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const noticesCarburetor = new NoticesCarburetor({ updateAvailable: null, agentError: null });
