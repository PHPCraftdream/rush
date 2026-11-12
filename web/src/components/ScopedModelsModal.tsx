import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { X } from "lucide-react";
import { configCarburetor } from "../state/config";
import { ws } from "../ws";
import { buildModelList } from "./modelList/buildModelList";
import type { ModelItem } from "./modelList/modelListTypes";
import type { Session, WSMessage } from "../types";
import { CustomRolesEditor } from "./CustomRolesEditor";
import { ScopedSlotsBlock } from "./scopedModels/ScopedSlotsBlock";
import { SessionSlotsBlock } from "./scopedModels/SessionSlotsBlock";
import type { ModelScope, ScopedModelsWire, Slot } from "./scopedModels/scopedModelsTypes";

/**
 * Indexes models by their provider:::model key.
 *
 * @param models - the catalog
 */
function indexByKey(models: readonly ModelItem[]): Map<string, ModelItem> {
  const entries = new Map<string, ModelItem>();
  for (const model of models) entries.set(model.key, model);
  return entries;
}

/** Props of the default-models dialog. */
interface IScopedModelsModalProps {
  onClose: () => void;
  activeSession: Session | null;
}

/** Local state of the default-models dialog. */
interface IScopedModelsModalState {
  scopedModels: ScopedModelsWire | null;
}

/** Dialog editing the model defaults at System, Folder and Session level, plus custom roles. */
export class ScopedModelsModal extends AntiHookComponent<IScopedModelsModalProps, IScopedModelsModalState> {
  public state: IScopedModelsModalState = { scopedModels: null };

  private detachScoped: (() => void) | null = null;

  /** Subscribes to scoped-model pushes and requests them; closes on Escape. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("scopedModels", this.subscribe, []);
    this.useEffect("escapeKey", this.bindEscape, [this.props.onClose]);
  }

  /** Starts listening for scoped_models and asks for the current ones; returns the teardown. */
  @bind
  protected subscribe(): () => void {
    this.detachScoped = ws.on("scoped_models", this.onScopedModels);
    ws.send("get_scoped_models", {});
    return this.unsubscribe;
  }

  /** Stops listening for scoped_models. */
  @bind
  protected unsubscribe(): void {
    this.detachScoped?.();
    this.detachScoped = null;
  }

  /**
   * Stores the pushed scoped models.
   *
   * @param msg - the pushed frame
   */
  @bind
  protected onScopedModels(msg: WSMessage): void {
    this.setState({ scopedModels: msg.payload as ScopedModelsWire });
  }

  /** Attaches the Escape handler; returns its detach function. */
  @bind
  protected bindEscape(): () => void {
    document.addEventListener("keydown", this.onKey);
    return this.unbindEscape;
  }

  /** Detaches the Escape handler. */
  @bind
  protected unbindEscape(): void {
    document.removeEventListener("keydown", this.onKey);
  }

  /**
   * Closes the dialog on Escape.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    if (e.key === "Escape") this.props.onClose();
  }

  /**
   * Keeps clicks inside the panel from reaching the backdrop.
   *
   * @param e - the click event
   */
  @bind
  protected stopPropagation(e: MouseEvent): void {
    e.stopPropagation();
  }

  // reasoning_effort is sent as "" for models with no effort knob, so the
  // backend stores nothing rather than a value the CLI would reject.
  /**
   * Sets a scoped model default.
   *
   * @param scope - global or workspace
   * @param slot - the slot
   * @param provider - provider id
   * @param model - model id
   * @param effort - reasoning effort, "" for none
   */
  @bind
  protected setScoped(scope: ModelScope, slot: Slot, provider: string, model: string, effort: string): void {
    ws.send("set_scoped_model", { scope, slot, provider, model, reasoning_effort: effort });
  }

  /**
   * Clears a scoped model default.
   *
   * @param scope - global or workspace
   * @param slot - the slot
   */
  @bind
  protected clearScoped(scope: ModelScope, slot: Slot): void {
    ws.send("clear_scoped_model", { scope, slot });
  }

  /** Renders the dialog. */
  public render() {
    const { onClose, activeSession } = this.props;
    const { scopedModels } = this.state;
    const { config } = this.useCarburetor(configCarburetor);
    const allModels = buildModelList(config);
    const modelByKey = indexByKey(allModels);

    return (
      <div
        className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
        onClick={onClose}
        data-test-id="scoped-models-modal-overlay"
      >
        <div
          className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-2xl overflow-hidden flex flex-col max-h-[85vh] chat-font"
          onClick={this.stopPropagation}
          data-test-id="scoped-models-modal"
        >
          <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0">
            <div>
              <h2 className="text-base font-semibold text-text">Default models</h2>
              <p className="text-xs text-text-subtle mt-0.5">
                Cascade: System → Folder → Session. An unset slot inherits from the level above.
              </p>
            </div>
            <button
              onClick={onClose}
              className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay"
            >
              <X size={16} />
            </button>
          </div>

          <div className="flex-1 overflow-y-auto px-5 py-4 space-y-6">
            {/* System block */}
            <section data-test-id="scoped-models-system">
              <h3 className="text-sm font-semibold text-text mb-1">System</h3>
              <p className="text-[11px] text-text-subtle mb-2">Global default — ~/.local/share/rush/rush.json</p>
              <ScopedSlotsBlock
                scope="global"
                models={allModels}
                modelByKey={modelByKey}
                scopedModels={scopedModels}
                onSet={this.setScoped}
                onClear={this.clearScoped}
              />
            </section>

            {/* Folder block */}
            <section data-test-id="scoped-models-folder">
              <h3 className="text-sm font-semibold text-text mb-1">Folder</h3>
              <p className="text-[11px] text-text-subtle mb-2">
                Workspace override — ./.rush/rush.json
                {scopedModels && !scopedModels.hasWorkspace && " (no workspace config resolved for this directory)"}
              </p>
              <ScopedSlotsBlock
                scope="workspace"
                models={allModels}
                modelByKey={modelByKey}
                scopedModels={scopedModels}
                onSet={this.setScoped}
                onClear={this.clearScoped}
                disabled={!!scopedModels && !scopedModels.hasWorkspace}
              />
            </section>

            <CustomRolesEditor models={allModels} />

            {/* Session block */}
            <section data-test-id="scoped-models-session">
              <h3 className="text-sm font-semibold text-text mb-1">Session</h3>
              {activeSession ? (
                <>
                  <p className="text-[11px] text-text-subtle mb-2">
                    Only affects the active session — {activeSession.Title}
                  </p>
                  <SessionSlotsBlock
                    models={allModels}
                    modelByKey={modelByKey}
                    session={activeSession}
                    scopedModels={scopedModels}
                  />
                </>
              ) : (
                <p className="text-[11px] text-text-subtle">
                  No active session — open a session to set per-session overrides.
                </p>
              )}
            </section>
          </div>
        </div>
      </div>
    );
  }
}
