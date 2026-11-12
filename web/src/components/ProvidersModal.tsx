import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { removeCustomProvider, type ConfigScope } from "../store";
import { X, Plus } from "lucide-react";
import type { ProviderInfo } from "../types";
import { wsRequest } from "../ws";
import { configCarburetor } from "../state/config";
import type { IConfigState } from "../state/ConfigCarburetor";
import { ProviderForm } from "./providers/ProviderForm";
import { ProviderRow } from "./providers/ProviderRow";
import type { IProviderFormData } from "./providers/providerFormTypes";
import { wireModels } from "./providers/wireModels";

/** Shared empty provider table for a missing config. */
const NO_PROVIDERS: TReadonly<Record<string, ProviderInfo>> = {};

/** One provider as a [id, info] pair. */
type ProviderEntry = [string, TReadonly<ProviderInfo>];

/**
 * Picks the provider table out of the config state.
 *
 * @param state - the config state
 */
const selectProviders = (state: TReadonly<IConfigState>) => state.config?.providers ?? NO_PROVIDERS;

/**
 * Orders providers: configured (API key set) first, then alphabetical within each group.
 *
 * @param a - first entry
 * @param b - second entry
 */
function byConfiguredThenID([a, infoA]: ProviderEntry, [b, infoB]: ProviderEntry): number {
  const configuredA = infoA.apiKeySet ? 0 : 1;
  const configuredB = infoB.apiKeySet ? 0 : 1;
  if (configuredA !== configuredB) return configuredA - configuredB;
  return a.localeCompare(b);
}

/** Props of the providers dialog. */
interface IProvidersModalProps {
  onClose: () => void;
}

/** Local state of the providers dialog. */
interface IProvidersModalState {
  showAdd: boolean;
}

/** Dialog listing every configured provider, with per-provider editing and custom-provider add/remove. */
export class ProvidersModal extends AntiHookComponent<IProvidersModalProps, IProvidersModalState> {
  public state: IProvidersModalState = { showAdd: false };

  /** Detached snapshot of the provider table, safe to hand to rows. */
  private readonly providersView = this.connectSelection(configCarburetor, selectProviders);

  /** Closes the dialog on Escape while mounted. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("escapeKey", this.bindEscape, [this.props.onClose]);
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

  /** Opens the add form. */
  @bind
  protected openAdd(): void {
    this.setState({ showAdd: true });
  }

  /** Closes the add form. */
  @bind
  protected closeAdd(): void {
    this.setState({ showAdd: false });
  }

  /**
   * Removes a custom provider's override at one scope.
   *
   * @param id - the provider id
   * @param scope - the scope to remove from
   */
  @bind
  protected removeProvider(id: string, scope: ConfigScope): void {
    removeCustomProvider(id, scope);
  }

  /**
   * Sends the new custom provider to the server.
   *
   * @param data - the submitted form values
   */
  @bind
  protected submitAdd(data: IProviderFormData): Promise<unknown> {
    return wsRequest("add_custom_provider", {
      id: data.id,
      name: data.name || undefined,
      type: data.type,
      baseUrl: data.baseUrl,
      apiKey: data.apiKey || undefined,
      models: wireModels(data.models),
      peakHours: data.peakHours,
      scope: data.scope,
    });
  }

  /**
   * Renders one provider row.
   *
   * @param entry - the [id, info] pair
   */
  @bind
  protected renderProvider([id, info]: ProviderEntry) {
    return <ProviderRow key={id} id={id} info={info} onRemove={this.removeProvider} />;
  }

  /** Renders the dialog. */
  public render() {
    const { onClose } = this.props;
    const { showAdd } = this.state;
    // Show every configured provider — built-in/catwalk-known (anthropic,
    // openai, zai, ...) as well as custom. Built-in providers only get the
    // peak-hours-only editor + no remove button (see ProviderRow); full
    // add/edit/remove stays custom-provider-only.
    const allProviders = Object.entries(this.providersView());

    return (
      <div
        className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
        onClick={onClose}
        data-test-id="providers-modal-overlay"
      >
        <div
          className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[80vh] chat-font"
          onClick={this.stopPropagation}
          data-test-id="providers-modal"
        >
          {/* Header */}
          <div
            className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0"
            data-test-id="providers-modal-header"
          >
            <div>
              <h2 className="text-base font-semibold text-text">Providers</h2>
              <p className="text-xs text-text-subtle mt-0.5">
                {allProviders.length === 0
                  ? "No providers configured"
                  : `${allProviders.length} provider${allProviders.length !== 1 ? "s" : ""}`}
              </p>
            </div>
            <button
              onClick={onClose}
              className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay"
            >
              <X size={16} />
            </button>
          </div>

          {/* List */}
          <div className="flex-1 overflow-y-auto">
            {allProviders.length === 0 ? (
              <div className="flex flex-col items-center justify-center py-12 px-6 text-center">
                <p className="text-text-muted text-sm font-medium mb-1">No providers</p>
                <p className="text-text-subtle text-xs">Add a provider to use Ollama, LM Studio, Deepseek, etc.</p>
              </div>
            ) : (
              allProviders.sort(byConfiguredThenID).map(this.renderProvider)
            )}
          </div>

          {/* Add form */}
          {showAdd ? (
            <ProviderForm submitLabel="Add Provider" onSubmit={this.submitAdd} onCancel={this.closeAdd} />
          ) : (
            <div className="border-t border-surface px-4 py-3 shrink-0">
              <button
                onClick={this.openAdd}
                className="flex items-center gap-2 w-full px-4 py-2.5 text-sm font-medium text-accent border border-accent/30 rounded-xl hover:bg-accent/5 transition-colors"
                data-test-id="providers-modal-add"
              >
                <Plus size={15} />
                Add custom provider
              </button>
            </div>
          )}
        </div>
      </div>
    );
  }
}
