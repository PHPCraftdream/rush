import { QueueCarburetor } from "./QueueCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const queueCarburetor = new QueueCarburetor({ messageQueue: {} });
