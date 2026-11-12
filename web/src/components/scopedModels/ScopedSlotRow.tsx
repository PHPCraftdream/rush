import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Undo2 } from "lucide-react";
import type { ModelItem } from "../modelList/modelListTypes";
import { EffortPicker } from "./EffortPicker";
import { ModelPicker } from "./ModelPicker";
import { inheritedText } from "./inheritedText";
import { pickedEffort } from "./pickedEffort";
import type { ModelScope, ScopedModelSlotWire, Slot } from "./scopedModelsTypes";

// ── One slot row within the System/Folder blocks ─────────────────────────────

/** Props of one scoped slot row. */
interface IScopedSlotRowProps {
  label: string;
  slot: Slot;
  models: ModelItem[];
  modelByKey: ReadonlyMap<string, ModelItem>;
  slotWire: ScopedModelSlotWire | undefined;
  scopeKey: ModelScope;
  onSet: (scope: ModelScope, slot: Slot, provider: string, model: string, effort: string) => void;
  onClear: (scope: ModelScope, slot: Slot) => void;
  disabled?: boolean;
}

/** One model slot of the System or Folder block. */
export class ScopedSlotRow extends AntiHookComponent<IScopedSlotRowProps> {
  /** This scope's explicit choice for the slot, if any. */
  protected explicit() {
    const { slotWire, scopeKey } = this.props;
    return scopeKey === "global" ? slotWire?.global : slotWire?.workspace;
  }

  /**
   * Stores a newly picked model, keeping the stored effort valid for it.
   *
   * @param p - provider id
   * @param m - model id
   */
  @bind
  protected onPick(p: string, m: string): void {
    const { modelByKey, scopeKey, slot, onSet } = this.props;
    const stored = this.explicit()?.reasoning_effort ?? "";
    onSet(scopeKey, slot, p, m, pickedEffort(p, m, stored, modelByKey.get(`${p}:::${m}`)));
  }

  /**
   * Stores a newly picked effort for the current model.
   *
   * @param eff - the effort
   */
  @bind
  protected onEffort(eff: string): void {
    const explicit = this.explicit();
    if (!explicit) return;
    const { scopeKey, slot, onSet } = this.props;
    onSet(scopeKey, slot, explicit.provider, explicit.model, eff);
  }

  /** Clears this scope's override. */
  @bind
  protected clear(): void {
    const { scopeKey, slot, onClear } = this.props;
    onClear(scopeKey, slot);
  }

  /** Renders the row. */
  public render() {
    const { label, models, modelByKey, slotWire, scopeKey, disabled } = this.props;
    const explicit = this.explicit();
    const effective = slotWire?.effective;
    const effectiveScope = slotWire?.effectiveScope;

    return (
      <div className="flex items-center gap-2 py-1.5">
        <span className="text-xs text-text-subtle w-28 shrink-0">{label}</span>
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-1.5">
            <ModelPicker
              models={models}
              value={explicit ? `${explicit.provider}:::${explicit.model}` : ""}
              onChange={this.onPick}
              disabled={disabled}
            />
            {explicit && (
              <EffortPicker
                provider={explicit.provider}
                model={explicit.model}
                capabilities={modelByKey.get(`${explicit.provider}:::${explicit.model}`)}
                value={explicit.reasoning_effort ?? ""}
                onChange={this.onEffort}
                disabled={disabled}
              />
            )}
          </div>
          {!explicit && (
            <p className="text-[10px] text-text-muted mt-0.5 truncate">
              {inheritedText(effective, effectiveScope)}
            </p>
          )}
        </div>
        {explicit && (
          <button
            onClick={this.clear}
            disabled={disabled}
            title={`Clear this ${scopeKey} override`}
            className="shrink-0 p-1.5 rounded-lg text-text-subtle hover:text-red hover:bg-red/10 transition-colors disabled:opacity-40"
          >
            <Undo2 size={13} />
          </button>
        )}
      </div>
    );
  }
}
