import type { Slot } from "./scopedModelsTypes";

/** The model slots in display order. */
export const SLOTS: { key: Slot; label: string }[] = [
  { key: "smart", label: "Smart (strong)" },
  { key: "fast", label: "Fast (cheap)" },
  { key: "worker", label: "Worker" },
  { key: "reviewer", label: "Reviewer" },
];
