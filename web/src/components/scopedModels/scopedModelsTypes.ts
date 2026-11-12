// ── Wire types (mirror internal/server/protocol.go) ─────────────────────────

/** One explicit model choice of a scope. */
export interface ModelOverrideWire {
  provider: string;
  model: string;
  reasoning_effort?: string;
}

/** One model slot across the global and workspace scopes. */
export interface ScopedModelSlotWire {
  global: ModelOverrideWire | null;
  workspace: ModelOverrideWire | null;
  effective: ModelOverrideWire | null;
  effectiveScope: "global" | "workspace" | "";
}

/** The scoped_models payload: every slot, plus whether a workspace config resolved. */
export interface ScopedModelsWire {
  smart: ScopedModelSlotWire;
  fast: ScopedModelSlotWire;
  worker: ScopedModelSlotWire;
  reviewer: ScopedModelSlotWire;
  hasWorkspace: boolean;
}

/** A model slot name. */
export type Slot = "smart" | "fast" | "worker" | "reviewer";

/** A config scope that stores model defaults. */
export type ModelScope = "global" | "workspace";
