import type { ChangeEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Plus, Trash2 } from "lucide-react";
import type { PeakHoursWindowSpec, ProviderPeakHoursPolicy } from "../types";
import { DAYS } from "./peakHours/DAYS";
import { MAX_WINDOWS } from "./peakHours/MAX_WINDOWS";
import { canonicalDays } from "./peakHours/canonicalDays";
import { defaultPolicy } from "./peakHours/defaultPolicy";
import { peakDaysSet } from "./peakHours/peakDaysSet";
import { peakHoursProblems } from "./peakHours/peakHoursProblems";
import { peakSummary } from "./peakHours/peakSummary";
import { toggleDay } from "./peakHours/toggleDay";

// ── Shared weekly peak-hours editor (task #56) ───────────────────────────────
//
// Used by BOTH the custom-provider form (idPrefix "provider-form-peak") and
// the built-in peak-hours-only editor (idPrefix "peak-hours-only"). The
// editor owns no provider identity and never talks to the wire — parents
// submit via set_provider_peak_hours / add_custom_provider /
// update_custom_provider — and it never decides whether a provider is
// currently blocked: there are deliberately no Date.now()-based active
// checks here. Status display is PeakHoursStatus's job, backed by the
// server's get_peak_hours_status reply.
//
// data-test-ids are parameterised by idPrefix so the existing Playwright
// specs keep working unchanged: provider-form-peak-toggle / -inputs /
// -start / -end / -message on the custom form, peak-hours-only-toggle /
// -inputs / -start / -end / -message on the built-in editor. The start/end
// test-ids land on the FIRST window row only.

const inputCls = "w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text";

/** Shared class of the small preset buttons. */
const presetCls = "px-1.5 py-0.5 text-[10px] text-text-subtle hover:text-text rounded-md border border-surface";

/**
 * Renders one problem hint.
 *
 * @param p - the problem text
 */
const renderProblem = (p: string) => <span key={p} className="block">{p}</span>;

/** Props of one weekday chip. */
interface IDayChipProps {
  day: string;
  active: boolean;
  onToggle: (day: string) => void;
}

/** One weekday toggle of a window row. */
class DayChip extends AntiHookComponent<IDayChipProps> {
  /** Reports this day as toggled. */
  @bind
  protected toggle(): void {
    this.props.onToggle(this.props.day);
  }

  /** Renders the chip. */
  public render() {
    const { day, active } = this.props;
    return (
      <button
        type="button"
        onClick={this.toggle}
        className={`px-1.5 py-0.5 text-[10px] uppercase rounded-md border transition-colors ${
          active
            ? "bg-accent-fill text-white/90 border-accent/30"
            : "text-text-subtle border-surface hover:text-text"
        }`}
      >
        {day.slice(0, 2)}
      </button>
    );
  }
}

/** Props of one days preset button. */
interface IPresetButtonProps {
  days: string;
  label: string;
  onPick: (days: string) => void;
}

/** A button applying a fixed days spec to a window row. */
class PresetButton extends AntiHookComponent<IPresetButtonProps> {
  /** Reports this preset as picked. */
  @bind
  protected pick(): void {
    this.props.onPick(this.props.days);
  }

  /** Renders the button. */
  public render() {
    return (
      <button type="button" onClick={this.pick} className={presetCls}>{this.props.label}</button>
    );
  }
}

/** Props of one window row. */
interface IPeakWindowRowProps {
  idPrefix: string;
  index: number;
  window: PeakHoursWindowSpec;
  windowCount: number;
  onUpdate: (index: number, field: keyof PeakHoursWindowSpec, value: string) => void;
  onSetDays: (index: number, days: string) => void;
  onRemove: (index: number) => void;
}

/** One weekly window: weekday chips, presets and start/end inputs. */
class PeakWindowRow extends AntiHookComponent<IPeakWindowRowProps> {
  /**
   * Flips one weekday of this window.
   *
   * @param day - the weekday token
   */
  @bind
  protected toggleChip(day: string): void {
    const { index, window, onSetDays } = this.props;
    onSetDays(index, toggleDay(window.days ?? "", day));
  }

  /**
   * Applies a preset days spec to this window.
   *
   * @param days - the spec
   */
  @bind
  protected pickPreset(days: string): void {
    this.props.onSetDays(this.props.index, days);
  }

  /** Removes this window. */
  @bind
  protected remove(): void {
    this.props.onRemove(this.props.index);
  }

  /**
   * Stores the typed start time.
   *
   * @param e - the input change event
   */
  @bind
  protected onStartChange(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onUpdate(this.props.index, "start", e.target.value);
  }

  /**
   * Stores the typed end time.
   *
   * @param e - the input change event
   */
  @bind
  protected onEndChange(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onUpdate(this.props.index, "end", e.target.value);
  }

  /**
   * Renders one weekday chip.
   *
   * @param d - the weekday token
   */
  @bind
  protected renderChip(d: string) {
    const active = peakDaysSet(this.props.window.days ?? "").has(d);
    return <DayChip key={d} day={d} active={active} onToggle={this.toggleChip} />;
  }

  /** Renders the window row. */
  public render() {
    const { idPrefix, index: i, window: w, windowCount } = this.props;
    return (
      <div className="space-y-1.5 p-2.5 bg-base-overlay border border-surface rounded-xl">
        <div className="flex items-center gap-1 flex-wrap">
          {DAYS.map(this.renderChip)}
          <span className="text-[10px] text-text-subtle ml-1">|</span>
          <PresetButton days="" label="All" onPick={this.pickPreset} />
          <PresetButton days="mon-fri" label="Mon–Fri" onPick={this.pickPreset} />
          <PresetButton days="sat,sun" label="Weekend" onPick={this.pickPreset} />
          {windowCount > 1 && (
            <button
              type="button"
              onClick={this.remove}
              title="Remove window"
              className="ml-auto text-text-subtle hover:text-red transition-colors"
              data-test-id={`${idPrefix}-remove-row`}
            >
              <Trash2 size={12} />
            </button>
          )}
        </div>
        <div className="grid grid-cols-2 gap-2">
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">Start {i === 0 ? "" : `(${i + 1})`}</label>
            <input
              type="text"
              // A plain HH:MM text field, not <input type="time"> — the
              // native time picker's AM/PM vs 24h display follows the OS
              // locale in most non-Chromium browsers, but peak_hours is
              // always 24h HH:MM server-side, so we own the format.
              inputMode="numeric"
              pattern="([01]\d|2[0-3]):[0-5]\d"
              placeholder="HH:MM"
              maxLength={5}
              value={w.start}
              onChange={this.onStartChange}
              className={inputCls}
              data-test-id={i === 0 ? `${idPrefix}-start` : undefined}
            />
          </div>
          <div>
            <label className="block text-[11px] text-text-subtle mb-1">
              End {i === 0 ? "" : `(${i + 1})`} (exclusive)
            </label>
            <input
              type="text"
              inputMode="numeric"
              pattern="([01]\d|2[0-3]):[0-5]\d|24:00"
              placeholder="HH:MM"
              maxLength={5}
              value={w.end}
              onChange={this.onEndChange}
              className={inputCls}
              data-test-id={i === 0 ? `${idPrefix}-end` : undefined}
            />
          </div>
        </div>
      </div>
    );
  }
}

/** Props of the weekly peak-hours editor. */
interface IPeakHoursEditorProps {
  idPrefix: string;
  value: ProviderPeakHoursPolicy | null;
  onChange: (next: ProviderPeakHoursPolicy | null) => void;
  pausedUntil?: string;
  onPause?: (duration?: string) => Promise<unknown> | unknown;
  onResume?: () => Promise<unknown> | unknown;
}

/** Local state of the weekly peak-hours editor. */
interface IPeakHoursEditorState {
  pauseDuration: string;
}

/** Weekly peak-hours policy editor with optional pause/resume controls. */
export class PeakHoursEditor extends AntiHookComponent<IPeakHoursEditorProps, IPeakHoursEditorState> {
  public state: IPeakHoursEditorState = { pauseDuration: "" };

  /**
   * Changes one field of one window.
   *
   * @param i - the window index
   * @param field - the field to change
   * @param v - the new value
   */
  @bind
  protected update(i: number, field: keyof PeakHoursWindowSpec, v: string): void {
    const { value, onChange } = this.props;
    const windows = value?.windows ?? [];
    const next = windows.map((w, idx) => (idx === i ? { ...w, [field]: v } : w));
    onChange({ ...(value ?? defaultPolicy()), windows: next });
  }

  /** Appends a default window. */
  @bind
  protected addRow(): void {
    const { value, onChange } = this.props;
    const windows = value?.windows ?? [];
    onChange({ ...(value ?? defaultPolicy()), windows: [...windows, { days: "", start: "09:00", end: "18:00" }] });
  }

  /**
   * Removes a window; removing the last one turns peak hours off.
   *
   * @param i - the window index
   */
  @bind
  protected removeRow(i: number): void {
    const { value, onChange } = this.props;
    const next = (value?.windows ?? []).filter((_, idx) => idx !== i);
    onChange(next.length > 0 ? { ...(value ?? defaultPolicy()), windows: next } : null);
  }

  /**
   * Sets a window's days, canonicalised.
   *
   * @param i - the window index
   * @param days - the days spec
   */
  @bind
  protected setDays(i: number, days: string): void {
    this.update(i, "days", canonicalDays(days));
  }

  /**
   * Turns peak hours on or off.
   *
   * @param e - the checkbox change event
   */
  @bind
  protected onToggle(e: ChangeEvent<HTMLInputElement>): void {
    const { value, onChange } = this.props;
    onChange(e.target.checked ? (value ?? defaultPolicy()) : null);
  }

  /** Turns peak hours off. */
  @bind
  protected clearAll(): void {
    this.props.onChange(null);
  }

  /**
   * Stores the typed timezone.
   *
   * @param e - the input change event
   */
  @bind
  protected onTimezoneChange(e: ChangeEvent<HTMLInputElement>): void {
    const { value, onChange } = this.props;
    onChange({ ...(value ?? defaultPolicy()), timezone: e.target.value });
  }

  /**
   * Stores the typed exit message.
   *
   * @param e - the textarea change event
   */
  @bind
  protected onMessageChange(e: ChangeEvent<HTMLTextAreaElement>): void {
    const { value, onChange } = this.props;
    onChange({ ...(value ?? defaultPolicy()), message: e.target.value });
  }

  /**
   * Stores the typed pause duration.
   *
   * @param e - the input change event
   */
  @bind
  protected onPauseDurationChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ pauseDuration: e.target.value });
  }

  /** Pauses; an empty input means undefined, so the server picks the end of the active band. */
  @bind
  protected pause(): void {
    this.props.onPause?.(this.state.pauseDuration.trim() || undefined);
    this.setState({ pauseDuration: "" });
  }

  /**
   * Renders one window row.
   *
   * @param w - the window
   * @param i - its index
   */
  @bind
  protected renderWindow(w: PeakHoursWindowSpec, i: number) {
    return (
      <PeakWindowRow
        key={i}
        idPrefix={this.props.idPrefix}
        index={i}
        window={w}
        windowCount={(this.props.value?.windows ?? []).length}
        onUpdate={this.update}
        onSetDays={this.setDays}
        onRemove={this.removeRow}
      />
    );
  }

  /** Renders the window rows with their add/clear controls, timezone, message and hints. */
  protected renderInputs() {
    const { idPrefix, value } = this.props;
    const windows = value?.windows ?? [];
    const problems = peakHoursProblems(value);
    return (
      <div className="space-y-2" data-test-id={`${idPrefix}-inputs`}>
        {windows.map(this.renderWindow)}
        <div className="flex items-center gap-3">
          <button
            type="button"
            onClick={this.addRow}
            className="flex items-center gap-1.5 text-xs text-accent hover:text-accent/80 transition-colors"
            data-test-id={`${idPrefix}-add-row`}
          >
            <Plus size={12} />
            Add window
          </button>
          <button
            type="button"
            onClick={this.clearAll}
            className="text-xs text-text-subtle hover:text-text transition-colors"
            data-test-id={`${idPrefix}-clear-all`}
          >
            Clear all
          </button>
          {windows.length > MAX_WINDOWS && (
            <span className="text-[10px] text-red">At most {MAX_WINDOWS} windows</span>
          )}
        </div>
        <div>
          <label htmlFor={`${idPrefix}-timezone`} className="block text-[11px] text-text-subtle mb-1">
            Timezone (optional)
          </label>
          <input
            id={`${idPrefix}-timezone`}
            type="text"
            value={value?.timezone ?? ""}
            onChange={this.onTimezoneChange}
            placeholder="local, UTC, Asia/Shanghai, UTC+08:00"
            className={inputCls}
            data-test-id={`${idPrefix}-timezone`}
          />
        </div>
        <div>
          <label htmlFor={`${idPrefix}-message`} className="block text-[11px] text-text-subtle mb-1">
            Exit message (optional)
          </label>
          <textarea
            id={`${idPrefix}-message`}
            rows={2}
            placeholder="Shown after a blank line when rush exits because of this window"
            value={value?.message ?? ""}
            onChange={this.onMessageChange}
            className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50 resize-none"
            data-test-id={`${idPrefix}-message`}
          />
        </div>
        {problems.length > 0 && (
          <p className="text-[10px] text-red">
            {problems.map(renderProblem)}
          </p>
        )}
        {windows.length > 0 && (
          <p className="text-[10px] text-text-muted">
            Weekly windows · {peakSummary(value)} · whether it's active right now is decided by the server
          </p>
        )}
      </div>
    );
  }

  /** Renders the pause/resume row. */
  protected renderPauseRow() {
    const { idPrefix, pausedUntil, onPause, onResume } = this.props;
    const { pauseDuration } = this.state;
    return (
      <div className="flex items-center gap-2 flex-wrap">
        {pausedUntil && onResume && (
          <>
            {/* pausedUntil is a backend RFC3339 string — formatted for
                display only; the server decides whether the pause is still
                in effect. */}
            <span className="text-[11px] text-text-subtle">
              Paused until {new Date(pausedUntil).toLocaleString()}
            </span>
            <button
              type="button"
              onClick={onResume}
              className="px-2 py-0.5 text-xs text-text-subtle hover:text-text rounded-lg border border-surface"
              data-test-id={`${idPrefix}-resume`}
            >
              Resume
            </button>
          </>
        )}
        {onPause && !pausedUntil && (
          <>
            <input
              type="text"
              value={pauseDuration}
              onChange={this.onPauseDurationChange}
              placeholder="2h / 7d / empty = until the current window ends"
              className="flex-1 min-w-0 text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
              data-test-id={`${idPrefix}-pause-duration`}
            />
            <button
              type="button"
              onClick={this.pause}
              className="px-2.5 py-1.5 text-xs text-text-subtle hover:text-text rounded-lg border border-surface"
              data-test-id={`${idPrefix}-pause`}
            >
              Pause
            </button>
          </>
        )}
      </div>
    );
  }

  /** Renders the toggle, the editor body when enabled, and the pause controls. */
  public render() {
    const { idPrefix, value, onPause, onResume } = this.props;
    const enabled = value !== null;
    return (
      <div className="space-y-2 border-t border-surface pt-3">
        <label className="flex items-center gap-2 text-[11px] text-text-subtle cursor-pointer select-none">
          <input
            type="checkbox"
            checked={enabled}
            onChange={this.onToggle}
            className="accent-accent"
            data-test-id={`${idPrefix}-toggle`}
          />
          Restrict to hours
        </label>
        {enabled && this.renderInputs()}
        {(onPause || onResume) && this.renderPauseRow()}
      </div>
    );
  }
}
