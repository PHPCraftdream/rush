import type { ChangeEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { X, Plus, AlertCircle, Loader2 } from "lucide-react";
import { pauseProviderPeakHours, resumeProviderPeakHours, type ConfigScope } from "../../store";
import type { ProviderPeakHoursPolicy } from "../../types";
import { PeakHoursEditor } from "../PeakHoursEditor";
import { legacyToPolicy } from "../peakHours/legacyToPolicy";
import { peakHoursProblems } from "../peakHours/peakHoursProblems";
import type { ModelDraft } from "./ModelDraft";
import { ModelEditor } from "./ModelEditor";
import { ScopeToggle } from "./ScopeToggle";
import { emptyModel } from "./emptyModel";
import type { IProviderFormData, IProviderFormInitial } from "./providerFormTypes";

const PROVIDER_TYPES = ["openai-compat", "openai", "anthropic", "gemini"];

/** Opaque background forced onto native select popups (see the comment at the Type select). */
const OPAQUE_BG = { backgroundColor: "rgb(var(--color-canvas))", color: "rgb(var(--color-text))" };

/**
 * The text of a thrown value.
 *
 * @param e - the thrown value
 */
const errorText = (e: unknown): string => (e instanceof Error ? e.message : String(e));

/**
 * Whether a model draft has both its id and its display name.
 *
 * @param m - the draft
 */
const isModelComplete = (m: ModelDraft): boolean => m.id.trim() !== "" && m.name.trim() !== "";

/**
 * Renders one provider-type option.
 *
 * @param t - the provider type
 */
const renderTypeOption = (t: string) => <option key={t} value={t} style={OPAQUE_BG}>{t}</option>;

/** Props of the provider form. */
interface IProviderFormProps {
  initial?: IProviderFormInitial;
  submitLabel: string;
  onSubmit: (data: IProviderFormData) => Promise<unknown>;
  onCancel: () => void;
}

/** Local state of the provider form. */
interface IProviderFormState {
  id: string;
  name: string;
  type: string;
  baseUrl: string;
  apiKey: string;
  models: ModelDraft[];
  // Whole peak-hours policy in one state value — null when the restriction
  // is off. legacyToPolicy maps old { start, end, message } wire data onto
  // the weekly policy so it edits fine and round-trips back as a policy.
  peak: ProviderPeakHoursPolicy | null;
  // Prefill from the provider's effective scope as reported by the server
  // wire — editing must write back to the scope the effective config
  // actually lives in, not silently create a global entry under a
  // still-active local override. A fresh add (no initial) defaults to
  // global, matching every scope-aware CLI command's default.
  scope: ConfigScope;
  error: string | null;
  busy: boolean;
}

/** Add/edit form of a custom provider. */
export class ProviderForm extends AntiHookComponent<IProviderFormProps, IProviderFormState> {
  public state: IProviderFormState = {
    id: this.props.initial?.id ?? "",
    name: this.props.initial?.name ?? "",
    type: this.props.initial?.type ?? "openai-compat",
    baseUrl: this.props.initial?.baseUrl ?? "",
    apiKey: "",
    models: this.props.initial?.models ?? [emptyModel()],
    peak: legacyToPolicy(this.props.initial?.peakHours),
    scope: this.props.initial?.scope ?? "global",
    error: null,
    busy: false,
  };

  // Continuation of submit() checks this flag after its await so a reply
  // landing after this form was cancelled cannot fire onCancel on a later
  // form (the wsRequest listeners themselves are always detached on
  // settle: reply, disconnect, timeout or a failed send).
  private disposed = false;

  /** Mirrors the live pause state into the draft and tracks mount state. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("syncPausedUntil", this.syncPausedUntil, [this.props.initial?.peakHours?.pausedUntil]);
    this.useEffect("lifecycle", this.markMounted, []);
  }

  /** Copies the live pausedUntil into the draft policy. */
  @bind
  protected syncPausedUntil(): void {
    const { peak } = this.state;
    if (peak) this.setState({ peak: { ...peak, pausedUntil: this.props.initial?.peakHours?.pausedUntil } });
  }

  /** Clears the disposed flag; returns the cleanup that sets it. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Marks the form as unmounted. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  // Pause/resume are real request/reply roundtrips: the store helper's
  // promise rejects on a server error, which surfaces in this form's
  // inline error line like any other failed write.
  /**
   * Pauses the provider's peak-hours restriction.
   *
   * @param duration - how long to pause; empty means until the active window ends
   */
  @bind
  protected async handlePause(duration?: string): Promise<void> {
    const { initial } = this.props;
    const { id, scope } = this.state;
    try {
      await pauseProviderPeakHours({ id: initial?.id ?? id, duration, scope });
    } catch (e) {
      this.setState({ error: errorText(e) });
    }
  }

  /** Resumes the provider's peak-hours restriction and clears the draft's pause. */
  @bind
  protected async handleResume(): Promise<void> {
    const { initial } = this.props;
    const { id, scope } = this.state;
    try {
      await resumeProviderPeakHours({ id: initial?.id ?? id, scope });
      const { peak } = this.state;
      if (!this.disposed && peak) this.setState({ peak: { ...peak, pausedUntil: undefined } });
    } catch (e) {
      this.setState({ error: errorText(e) });
    }
  }

  /** The first problem of the draft, or null when it can be submitted. */
  protected validate(): string | null {
    const { id, baseUrl, models, peak } = this.state;
    if (!id.trim()) return "Provider ID is required";
    if (!baseUrl.trim()) return "Base URL is required";
    if (!/^https?:\/\//.test(baseUrl.trim())) return "Base URL must start with http:// or https://";
    if (models.length === 0) return "At least one model is required";
    for (const m of models) {
      if (!m.id.trim()) return "Each model must have an ID";
      if (!m.name.trim()) return "Each model must have a display name";
    }
    if (peak && peakHoursProblems(peak).length > 0) return peakHoursProblems(peak)[0];
    return null;
  }

  /** Validates and submits; closes the form on success. */
  @bind
  protected async submit(): Promise<void> {
    const err = this.validate();
    if (err) {
      this.setState({ error: err });
      return;
    }
    const { onSubmit, onCancel } = this.props;
    const { id, name, type, baseUrl, apiKey, models, peak, scope } = this.state;
    this.setState({ error: null, busy: true });
    try {
      await onSubmit({
        id: id.trim(),
        name: name.trim(),
        type,
        baseUrl: baseUrl.trim(),
        apiKey,
        models,
        peakHours: peak,
        scope,
      });
      if (this.disposed) return;
      this.setState({ busy: false });
      onCancel();
    } catch (e) {
      if (this.disposed) return;
      this.setState({ busy: false, error: errorText(e) });
    }
  }

  /** Click handler of the submit button. */
  @bind
  protected onSubmitClick(): void {
    void this.submit();
  }

  /**
   * Stores the typed provider id.
   *
   * @param e - the input change event
   */
  @bind
  protected onIdChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ id: e.target.value });
  }

  /**
   * Stores the typed display name.
   *
   * @param e - the input change event
   */
  @bind
  protected onNameChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ name: e.target.value });
  }

  /**
   * Stores the typed base URL.
   *
   * @param e - the input change event
   */
  @bind
  protected onBaseUrlChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ baseUrl: e.target.value });
  }

  /**
   * Stores the chosen provider type.
   *
   * @param e - the select change event
   */
  @bind
  protected onTypeChange(e: ChangeEvent<HTMLSelectElement>): void {
    this.setState({ type: e.target.value });
  }

  /**
   * Stores the typed API key.
   *
   * @param e - the input change event
   */
  @bind
  protected onApiKeyChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ apiKey: e.target.value });
  }

  /**
   * Stores the edited model drafts.
   *
   * @param models - the new drafts
   */
  @bind
  protected onModelsChange(models: ModelDraft[]): void {
    this.setState({ models });
  }

  /**
   * Stores the edited peak-hours policy.
   *
   * @param peak - the new policy, or null when off
   */
  @bind
  protected onPeakChange(peak: ProviderPeakHoursPolicy | null): void {
    this.setState({ peak });
  }

  /**
   * Stores the chosen config scope.
   *
   * @param scope - the scope
   */
  @bind
  protected onScopeChange(scope: ConfigScope): void {
    this.setState({ scope });
  }

  /** Renders the form. */
  public render() {
    const { initial, submitLabel, onCancel } = this.props;
    const { id, name, type, baseUrl, apiKey, models, peak, scope, error, busy } = this.state;
    const canSubmit = !busy &&
      id.trim() !== "" &&
      baseUrl.trim() !== "" &&
      models.length > 0 &&
      models.every(isModelComplete) &&
      (peak === null || peakHoursProblems(peak).length === 0);

    return (
      <div className="border-t border-surface bg-base-subtle/50 p-4 space-y-3">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold text-text">
            {initial ? `Edit: ${initial.id}` : "Add Custom Provider"}
          </h3>
          <button onClick={onCancel} className="text-text-subtle hover:text-text transition-colors">
            <X size={15} />
          </button>
        </div>

        <div className="grid grid-cols-2 gap-2">
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">Provider ID *</label>
            <input
              value={id}
              onChange={this.onIdChange}
              placeholder="e.g. ollama"
              disabled={!!initial}
              className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50 disabled:opacity-60"
            />
          </div>
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">Display Name</label>
            <input
              value={name}
              onChange={this.onNameChange}
              placeholder="e.g. Ollama"
              className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
            />
          </div>
        </div>

        <div>
          <label className="block text-[11px] text-text-subtle mb-1">Scope</label>
          <ScopeToggle testId="provider-form-scope" scope={scope} onChange={this.onScopeChange} />
          <p className="text-[10px] text-text-muted mt-1">
            {scope === "global" ? "Available in every project (~/.local/share/rush)." : "This project only (./.rush)."}
          </p>
        </div>

        <div>
          <label className="block text-[11px] text-text-subtle mb-1">Base URL *</label>
          <input
            value={baseUrl}
            onChange={this.onBaseUrlChange}
            placeholder="e.g. http://localhost:11434/v1/"
            className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
          />
        </div>

        <div className="grid grid-cols-2 gap-2">
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">Type</label>
            <select
              value={type}
              onChange={this.onTypeChange}
              // Native <select>/<option> popups are rendered by the OS/browser
              // chrome, not by our stylesheet — Tailwind's bg-canvas class
              // alone leaves the dropdown list translucent on some platforms.
              // Force an explicit opaque background inline so the popup
              // itself (not just the closed control) is solid.
              style={OPAQUE_BG}
              className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
            >
              {PROVIDER_TYPES.map(renderTypeOption)}
            </select>
          </div>
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">API Key</label>
            <input
              type="password"
              value={apiKey}
              onChange={this.onApiKeyChange}
              placeholder={initial ? "Leave blank to keep existing" : "Optional"}
              className="w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
            />
          </div>
        </div>

        <div>
          <label className="block text-[11px] text-text-subtle mb-2">Models</label>
          <ModelEditor models={models} onChange={this.onModelsChange} />
        </div>

        <PeakHoursEditor
          idPrefix="provider-form-peak"
          value={peak}
          onChange={this.onPeakChange}
          pausedUntil={peak?.pausedUntil}
          onPause={this.handlePause}
          onResume={this.handleResume}
        />

        {error && (
          <p className="text-xs text-red flex items-center gap-1.5">
            <AlertCircle size={12} />
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <button
            onClick={onCancel}
            className="px-3 py-1.5 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
          >
            Cancel
          </button>
          <button
            onClick={this.onSubmitClick}
            disabled={!canSubmit}
            className="px-4 py-1.5 text-xs font-semibold bg-accent-fill text-white/90 rounded-lg hover:opacity-90 disabled:opacity-40 flex items-center gap-1.5"
          >
            {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}
            {busy ? "Saving…" : submitLabel}
          </button>
        </div>
      </div>
    );
  }
}
