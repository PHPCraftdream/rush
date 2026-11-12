import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { ModelItem } from "../modelList/modelListTypes";
import { SLOTS } from "./SLOTS";
import { ScopedSlotRow } from "./ScopedSlotRow";
import type { ModelScope, ScopedModelsWire, Slot } from "./scopedModelsTypes";

/** Props of a scoped slots block. */
interface IScopedSlotsBlockProps {
  scope: ModelScope;
  models: ModelItem[];
  modelByKey: ReadonlyMap<string, ModelItem>;
  scopedModels: ScopedModelsWire | null;
  onSet: (scope: ModelScope, slot: Slot, provider: string, model: string, effort: string) => void;
  onClear: (scope: ModelScope, slot: Slot) => void;
  disabled?: boolean;
}

/** The four slot rows of the System or Folder block. */
export class ScopedSlotsBlock extends AntiHookComponent<IScopedSlotsBlockProps> {
  /**
   * Renders one slot row.
   *
   * @param slotDef - the slot key and label
   * @param slotDef.key - the slot name
   * @param slotDef.label - its label
   */
  @bind
  protected renderSlot({ key, label }: { key: Slot; label: string }) {
    const { scope, models, modelByKey, scopedModels, onSet, onClear, disabled } = this.props;
    return (
      <ScopedSlotRow
        key={key}
        label={label}
        slot={key}
        models={models}
        modelByKey={modelByKey}
        slotWire={scopedModels?.[key]}
        scopeKey={scope}
        onSet={onSet}
        onClear={onClear}
        disabled={disabled}
      />
    );
  }

  /** Renders the rows. */
  public render() {
    return (
      <div className="divide-y divide-surface/30">
        {SLOTS.map(this.renderSlot)}
      </div>
    );
  }
}
