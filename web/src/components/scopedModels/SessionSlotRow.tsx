import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Undo2 } from "lucide-react";
import { clearSessionModelSlot } from "../../store";
import type { Session } from "../../types";
import type { ModelItem } from "../modelList/modelListTypes";
import { EffortPicker } from "./EffortPicker";
import { ModelPicker } from "./ModelPicker";
import { inheritedText } from "./inheritedText";
import { pickedEffort } from "./pickedEffort";
import { setSessionModelSlot } from "./setSessionModelSlot";
import type { ScopedModelsWire, Slot } from "./scopedModelsTypes";

// ── One slot row within the Session block ─────────────────────────────────────

/** Props of one session slot row. */
interface ISessionSlotRowProps {
  label: string;
  slot: Slot;
  models: ModelItem[];
  modelByKey: ReadonlyMap<string, ModelItem>;
  session: Session;
  scopedModels: ScopedModelsWire | null;
}

/** One model slot of the Session block. */
export class SessionSlotRow extends AntiHookComponent<ISessionSlotRowProps> {
  /** The session's columns for this slot. */
  protected fields() {
    const { slot, session } = this.props;
    const prefix = `${slot[0].toUpperCase()}${slot.slice(1)}`;
    const providerField = `${prefix}ModelProvider` as keyof Session;
    const idField = `${prefix}ModelID` as keyof Session;
    const effortField = `${prefix}ModelReasoningEffort` as keyof Session;
    return {
      provider: session[providerField] as string,
      modelID: session[idField] as string,
      storedEffort: (session[effortField] as string) ?? "",
    };
  }

  /**
   * Stores a newly picked model, keeping the stored effort valid for it.
   *
   * @param p - provider id
   * @param m - model id
   */
  @bind
  protected onPick(p: string, m: string): void {
    const { modelByKey, session, slot } = this.props;
    const { storedEffort } = this.fields();
    setSessionModelSlot(session.ID, slot, p, m, pickedEffort(p, m, storedEffort, modelByKey.get(`${p}:::${m}`)));
  }

  /**
   * Stores a newly picked effort for the current model.
   *
   * @param eff - the effort
   */
  @bind
  protected onEffort(eff: string): void {
    const { session, slot } = this.props;
    const { provider, modelID } = this.fields();
    setSessionModelSlot(session.ID, slot, provider, modelID, eff);
  }

  /** Clears this session's override. */
  @bind
  protected clear(): void {
    clearSessionModelSlot(this.props.session.ID, this.props.slot);
  }

  /** Renders the row. */
  public render() {
    const { label, slot, models, modelByKey, scopedModels } = this.props;
    const { provider, modelID, storedEffort } = this.fields();
    const hasOverride = !!(provider && modelID);

    const inherited = scopedModels?.[slot]?.effective;
    const inheritedScope = scopedModels?.[slot]?.effectiveScope;

    return (
      <div className="flex items-center gap-2 py-1.5">
        <span className="text-xs text-text-subtle w-28 shrink-0">{label}</span>
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-1.5">
            <ModelPicker
              models={models}
              value={hasOverride ? `${provider}:::${modelID}` : ""}
              onChange={this.onPick}
            />
            {hasOverride && (
              <EffortPicker
                provider={provider}
                model={modelID}
                capabilities={modelByKey.get(`${provider}:::${modelID}`)}
                value={storedEffort}
                onChange={this.onEffort}
              />
            )}
          </div>
          {!hasOverride && (
            <p className="text-[10px] text-text-muted mt-0.5 truncate">
              {inheritedText(inherited, inheritedScope)}
            </p>
          )}
        </div>
        {hasOverride && (
          <button
            onClick={this.clear}
            title="Clear this session's override"
            className="shrink-0 p-1.5 rounded-lg text-text-subtle hover:text-red hover:bg-red/10 transition-colors"
          >
            <Undo2 size={13} />
          </button>
        )}
      </div>
    );
  }
}
