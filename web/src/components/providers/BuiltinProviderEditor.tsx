import type { ChangeEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { X, AlertCircle, Loader2 } from "lucide-react";
import { pauseProviderPeakHours, resumeProviderPeakHours, type ConfigScope } from "../../store";
import type { ProviderInfo, ProviderPeakHoursPolicy } from "../../types";
import { wsRequest } from "../../ws";
import { PeakHoursEditor } from "../PeakHoursEditor";
import { legacyToPolicy } from "../peakHours/legacyToPolicy";
import { peakHoursProblems } from "../peakHours/peakHoursProblems";
import { ScopeToggle } from "./ScopeToggle";

/**
 * The text of a thrown value.
 *
 * @param e - the thrown value
 */
const errorText = (e: unknown): string => (e instanceof Error ? e.message : String(e));

/**
 * The pausedUntil of a peak-hours wire value; only the policy shape carries it.
 *
 * `in` narrows the peakHours wire union, so this stays strict-mode clean without a cast.
 *
 * @param initial - the wire value
 */
const pausedUntilOf = (initial: TReadonly<ProviderInfo["peakHours"]>): string | undefined =>
  initial && "pausedUntil" in initial ? initial.pausedUntil : undefined;

/** Props of the built-in provider editor. */
interface IBuiltinProviderEditorProps {
  id: string;
  initial: TReadonly<ProviderInfo["peakHours"]>;
  initialScope?: ConfigScope;
  apiKeySet?: boolean;
  onCancel: () => void;
}

/** Local state of the built-in provider editor. */
interface IBuiltinProviderEditorState {
  // Same shared weekly editor as the custom form; null = restriction off.
  // legacyToPolicy maps old { start, end, message } data onto the policy.
  peak: ProviderPeakHoursPolicy | null;
  scope: ConfigScope;
  apiKey: string;
  error: string | null;
  busy: boolean;
}

// ── Peak-hours-only editor (built-in providers) ───────────────────────────────
//
// Built-in/catwalk-known providers (anthropic, openai, zai, ...) have a
// fixed type/baseUrl/model-list the client doesn't own — ProviderForm's full
// add/edit flow (which replaces every field) is only safe for custom
// providers. This editor sends targeted single-field writes instead
// (set_provider_peak_hours, set_provider_key/remove_provider_key), so a
// built-in provider's API key and peak_hours can be managed without
// touching anything else about it (type, base URL, model list).

/** Editor of a built-in provider's API key and peak hours. */
export class BuiltinProviderEditor extends AntiHookComponent<IBuiltinProviderEditorProps, IBuiltinProviderEditorState> {
  public state: IBuiltinProviderEditorState = {
    peak: legacyToPolicy(this.props.initial),
    scope: this.props.initialScope ?? "global",
    apiKey: "",
    error: null,
    busy: false,
  };

  // Continuations of submit()/removeKey() check this flag after their
  // await so a reply landing after this editor was cancelled cannot fire
  // onCancel on whatever editor is mounted next (the wsRequest listeners
  // themselves are always detached on settle: reply, disconnect,
  // timeout or a failed send).
  private disposed = false;

  /** Mirrors the live pause state into the draft and tracks mount state. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("syncPausedUntil", this.syncPausedUntil, [pausedUntilOf(this.props.initial)]);
    this.useEffect("lifecycle", this.markMounted, []);
  }

  /** Copies the live pausedUntil into the draft policy. */
  @bind
  protected syncPausedUntil(): void {
    const { peak } = this.state;
    if (peak) this.setState({ peak: { ...peak, pausedUntil: pausedUntilOf(this.props.initial) } });
  }

  /** Clears the disposed flag; returns the cleanup that sets it. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Marks the editor as unmounted. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  /** Validates and saves the peak hours and, when typed, the API key; closes on success. */
  @bind
  protected async submit(): Promise<void> {
    const { id, onCancel } = this.props;
    const { peak, scope, apiKey } = this.state;
    if (peak && peakHoursProblems(peak).length > 0) {
      this.setState({ error: peakHoursProblems(peak)[0] });
      return;
    }
    this.setState({ error: null, busy: true });
    try {
      await Promise.all([
        wsRequest("set_provider_peak_hours", { id, peakHours: peak, scope }),
        ...(apiKey.trim() ? [wsRequest("set_provider_key", { providerID: id, apiKey: apiKey.trim() })] : []),
      ]);
      if (this.disposed) return;
      this.setState({ busy: false });
      onCancel();
    } catch (e) {
      if (this.disposed) return;
      this.setState({ busy: false, error: errorText(e) });
    }
  }

  // Show roundtrip errors inline; broadcasts update only the draft pause field.
  /**
   * Pauses the provider's peak-hours restriction.
   *
   * @param duration - how long to pause; empty means until the active window ends
   */
  @bind
  protected async handlePause(duration?: string): Promise<void> {
    try {
      await pauseProviderPeakHours({ id: this.props.id, duration, scope: this.state.scope });
    } catch (e) {
      this.setState({ error: errorText(e) });
    }
  }

  /** Resumes the provider's peak-hours restriction and clears the draft's pause. */
  @bind
  protected async handleResume(): Promise<void> {
    try {
      await resumeProviderPeakHours({ id: this.props.id, scope: this.state.scope });
      const { peak } = this.state;
      if (!this.disposed && peak) this.setState({ peak: { ...peak, pausedUntil: undefined } });
    } catch (e) {
      this.setState({ error: errorText(e) });
    }
  }

  /** Removes the provider's stored API key. */
  @bind
  protected async removeKey(): Promise<void> {
    this.setState({ busy: true });
    try {
      await wsRequest("remove_provider_key", { providerID: this.props.id });
      if (this.disposed) return;
      this.setState({ busy: false });
    } catch (e) {
      if (this.disposed) return;
      this.setState({ busy: false, error: errorText(e) });
    }
  }

  /** Click handler of the Save button. */
  @bind
  protected onSaveClick(): void {
    void this.submit();
  }

  /** Click handler of the Remove key button. */
  @bind
  protected onRemoveKeyClick(): void {
    void this.removeKey();
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

  /** Renders the editor. */
  public render() {
    const { id, apiKeySet, onCancel } = this.props;
    const { peak, scope, apiKey, error, busy } = this.state;
    const canSubmit = !busy && (peak === null || peakHoursProblems(peak).length === 0);

    return (
      <div className="border-t border-surface bg-base-subtle/50 p-4 space-y-3" data-test-id="builtin-provider-editor">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold text-text">Edit: {id}</h3>
          <button onClick={onCancel} className="text-text-subtle hover:text-text transition-colors">
            <X size={15} />
          </button>
        </div>

        <div>
          <label className="block text-[11px] text-text-subtle mb-1">API key</label>
          <div className="flex gap-2">
            <input
              type="password"
              value={apiKey}
              onChange={this.onApiKeyChange}
              placeholder={apiKeySet ? "•••••••• (leave blank to keep)" : "sk-…"}
              className="flex-1 text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text"
              data-test-id="peak-hours-only-api-key"
            />
            {apiKeySet && (
              <button
                onClick={this.onRemoveKeyClick}
                disabled={busy}
                title="Remove API key"
                className="px-2.5 py-1.5 text-xs text-text-subtle hover:text-red transition-colors rounded-lg border border-surface disabled:opacity-40"
                data-test-id="peak-hours-only-remove-key"
              >
                Remove
              </button>
            )}
          </div>
        </div>

        <div>
          <label className="block text-[11px] text-text-subtle mb-1">Peak-hours scope</label>
          <ScopeToggle testId="peak-hours-only-scope" scope={scope} onChange={this.onScopeChange} />
        </div>

        <PeakHoursEditor
          idPrefix="peak-hours-only"
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
            onClick={this.onSaveClick}
            disabled={!canSubmit}
            className="px-4 py-1.5 text-xs font-semibold bg-accent-fill text-white/90 rounded-lg hover:opacity-90 disabled:opacity-40 flex items-center gap-1.5"
            data-test-id="peak-hours-only-save"
          >
            {busy ? <Loader2 size={12} className="animate-spin" /> : null}
            {busy ? "Saving…" : "Save"}
          </button>
        </div>
      </div>
    );
  }
}
