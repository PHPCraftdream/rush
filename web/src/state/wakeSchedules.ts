import { WakeSchedulesCarburetor } from "./WakeSchedulesCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const wakeSchedulesCarburetor = new WakeSchedulesCarburetor({ bySession: {} });
