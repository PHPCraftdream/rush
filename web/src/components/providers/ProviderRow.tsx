import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { Trash2, Pencil, CheckCircle2, ChevronDown, ChevronRight } from "lucide-react";
import type { ConfigScope } from "../../store";
import type { ProviderInfo } from "../../types";
import { wsRequest } from "../../ws";
import { PeakHoursStatus } from "../PeakHoursStatus";
import { legacyToPolicy } from "../peakHours/legacyToPolicy";
import { peakSummaryText } from "../peakHours/peakSummaryText";
import { BuiltinProviderEditor } from "./BuiltinProviderEditor";
import type { ModelDraft } from "./ModelDraft";
import { ProviderForm } from "./ProviderForm";
import type { IProviderFormData, IProviderFormInitial } from "./providerFormTypes";
import { wireModels } from "./wireModels";

/** One model of a provider as the server reports it. */
type WireModel = NonNullable<ProviderInfo["models"]>[number];

/**
 * Seeds a model draft from a server-reported model.
 *
 * @param m - the reported model
 */
const draftFromModel = (m: TReadonly<WireModel>): ModelDraft => ({
  id: m.id,
  name: m.name,
  contextWindow: m.contextWindow ? String(m.contextWindow) : "",
  costPer1mIn: "",
  costPer1mOut: "",
});

/**
 * Builds the values the edit form starts from.
 *
 * @param id - the provider id
 * @param info - the provider as the server reports it
 */
function buildFormInitial(id: string, info: TReadonly<ProviderInfo>): IProviderFormInitial {
  return {
    id,
    name: info.name ?? id,
    type: info.type ?? "openai-compat",
    baseUrl: info.baseUrl ?? "",
    models: (info.models ?? []).map(draftFromModel),
    peakHours: legacyToPolicy(info.peakHours),
    scope: info.scope,
  };
}

/**
 * Renders one model name chip.
 *
 * @param m - the reported model
 */
const renderModelChip = (m: TReadonly<WireModel>) => (
  <span
    key={m.id}
    className="text-[11px] font-mono text-text-muted bg-base-overlay border border-surface rounded-md px-2 py-0.5"
  >
    {m.name || m.id}
  </span>
);

/** Props of one provider row. */
interface IProviderRowProps {
  id: string;
  info: TReadonly<ProviderInfo>;
  onRemove: (id: string, scope: ConfigScope) => void;
}

/** Local state of one provider row. */
interface IProviderRowState {
  expanded: boolean;
  confirmRemove: boolean;
  editing: boolean;
}

/** One provider of the list: summary, edit, and (for custom providers) remove. */
export class ProviderRow extends AntiHookComponent<IProviderRowProps, IProviderRowState> {
  public state: IProviderRowState = { expanded: false, confirmRemove: false, editing: false };

  /** Expands or collapses the model list, when there are models. */
  @bind
  protected toggleExpanded(): void {
    if ((this.props.info.models ?? []).length > 0) this.setState({ expanded: !this.state.expanded });
  }

  /** Opens the editor. */
  @bind
  protected startEditing(): void {
    this.setState({ editing: true });
  }

  /** Closes the editor. */
  @bind
  protected stopEditing(): void {
    this.setState({ editing: false });
  }

  /** Asks which scope to remove from. */
  @bind
  protected askRemove(): void {
    this.setState({ confirmRemove: true });
  }

  /** Dismisses the remove prompt. */
  @bind
  protected cancelRemove(): void {
    this.setState({ confirmRemove: false });
  }

  /** Removes the global override. */
  @bind
  protected removeGlobal(): void {
    this.props.onRemove(this.props.id, "global");
    this.setState({ confirmRemove: false });
  }

  /** Removes the local override. */
  @bind
  protected removeLocal(): void {
    this.props.onRemove(this.props.id, "local");
    this.setState({ confirmRemove: false });
  }

  /**
   * Sends the edited custom provider to the server.
   *
   * @param data - the submitted form values
   */
  @bind
  protected submitUpdate(data: IProviderFormData): Promise<unknown> {
    return wsRequest("update_custom_provider", {
      oldId: this.props.id,
      id: data.id,
      name: data.name,
      type: data.type,
      baseUrl: data.baseUrl,
      apiKey: data.apiKey || undefined,
      models: wireModels(data.models),
      // Send null explicitly when cleared so the server clears it.
      peakHours: data.peakHours,
      scope: data.scope,
    });
  }

  /** Renders the remove prompt or the remove button of a custom provider. */
  protected renderRemove() {
    if (this.state.confirmRemove) {
      return (
        <div className="flex items-center gap-1 ml-1">
          <span className="text-xs text-text-subtle">Remove from:</span>
          <button
            onClick={this.removeGlobal}
            title="Remove the global (~/.local/share/rush) override"
            className="px-2 py-0.5 text-xs font-medium bg-red-fill text-white/90 rounded hover:opacity-90"
            data-test-id="provider-remove-global"
          >Global</button>
          <button
            onClick={this.removeLocal}
            title="Remove the local (./.rush) override"
            className="px-2 py-0.5 text-xs font-medium bg-red-fill/70 text-white/90 rounded hover:opacity-90"
            data-test-id="provider-remove-local"
          >Local</button>
          <button
            onClick={this.cancelRemove}
            className="px-2 py-0.5 text-xs text-text-subtle hover:text-text rounded"
          >No</button>
        </div>
      );
    }
    return (
      <button
        onClick={this.askRemove}
        title="Remove provider"
        className="p-1 text-text-subtle hover:text-red transition-colors rounded"
      >
        <Trash2 size={14} />
      </button>
    );
  }

  /** Renders the row. */
  public render() {
    const { id, info } = this.props;
    const { expanded, editing } = this.state;
    const models = info.models ?? [];
    const isCustom = !!info.isCustom;
    const expandTone =
      models.length > 0 ? "text-text-subtle hover:text-text cursor-pointer" : "text-surface cursor-default";

    if (editing && !isCustom) {
      return (
        <div className="border-b border-surface last:border-0">
          <BuiltinProviderEditor
            id={id}
            initial={info.peakHours}
            initialScope={info.scope}
            apiKeySet={info.apiKeySet}
            onCancel={this.stopEditing}
          />
        </div>
      );
    }

    if (editing) {
      return (
        <div className="border-b border-surface last:border-0">
          <ProviderForm
            initial={buildFormInitial(id, info)}
            submitLabel="Update Provider"
            onSubmit={this.submitUpdate}
            onCancel={this.stopEditing}
          />
        </div>
      );
    }

    return (
      <div className="border-b border-surface last:border-0" data-test-id="provider-row" data-provider-id={id}>
        <div className="flex items-center gap-3 px-4 py-3">
          <button
            onClick={this.toggleExpanded}
            className={`shrink-0 transition-colors ${expandTone}`}
          >
            {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          </button>

          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2">
              <span className="text-sm font-semibold text-text truncate">{info.name || id}</span>
              <span className="text-[11px] font-mono text-text-muted">{id}</span>
              {info.apiKeySet && (
                <span className="inline-flex items-center gap-1 text-[11px] text-green bg-green/10 border border-green/20 rounded-full px-2 py-0.5">
                  <CheckCircle2 size={10} />
                  Key set
                </span>
              )}
              {info.peakHours && (
                <PeakHoursStatus
                  providerId={id}
                  configured={!!info.peakHours}
                  summary={peakSummaryText(info.peakHours)}
                />
              )}
            </div>
            <div className="text-[11px] text-text-subtle font-mono truncate">
              {info.type ?? "openai-compat"}
              {info.baseUrl ? ` · ${info.baseUrl}` : ""}
              {models.length > 0 ? ` · ${models.length} model${models.length !== 1 ? "s" : ""}` : ""}
            </div>
          </div>

          <div className="flex items-center gap-1 shrink-0">
            <button
              onClick={this.startEditing}
              title="Edit provider"
              data-test-id={`provider-edit-${id}`}
              className="p-1 text-text-subtle hover:text-accent transition-colors rounded"
            >
              <Pencil size={14} />
            </button>
            {isCustom && this.renderRemove()}
          </div>
        </div>

        {expanded && models.length > 0 && (
          <div className="px-4 pb-3 pl-10">
            <div className="flex flex-wrap gap-1.5">
              {models.map(renderModelChip)}
            </div>
          </div>
        )}
      </div>
    );
  }
}
