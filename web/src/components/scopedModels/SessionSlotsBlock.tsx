import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { Session } from "../../types";
import type { ModelItem } from "../modelList/modelListTypes";
import { SLOTS } from "./SLOTS";
import { SessionSlotRow } from "./SessionSlotRow";
import type { ScopedModelsWire, Slot } from "./scopedModelsTypes";

/** Props of the session slots block. */
interface ISessionSlotsBlockProps {
  models: ModelItem[];
  modelByKey: ReadonlyMap<string, ModelItem>;
  session: Session;
  scopedModels: ScopedModelsWire | null;
}

/** The four slot rows of the Session block. */
export class SessionSlotsBlock extends AntiHookComponent<ISessionSlotsBlockProps> {
  /**
   * Renders one slot row.
   *
   * @param slotDef - the slot key and label
   * @param slotDef.key - the slot name
   * @param slotDef.label - its label
   */
  @bind
  protected renderSlot({ key, label }: { key: Slot; label: string }) {
    const { models, modelByKey, session, scopedModels } = this.props;
    return (
      <SessionSlotRow
        key={key}
        label={label}
        slot={key}
        models={models}
        modelByKey={modelByKey}
        session={session}
        scopedModels={scopedModels}
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
