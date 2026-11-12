import type { ChangeEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { effortLevelsFor, clampEffort, defaultEffortFor } from "../../effort";
import type { ModelItem } from "../modelList/modelListTypes";

/**
 * Renders one effort option.
 *
 * @param l - the effort level
 */
const renderLevel = (l: string) => <option key={l} value={l}>{l}</option>;

/** Props of the effort picker. */
interface IEffortPickerProps {
  provider: string;
  model: string;
  capabilities?: ModelItem;
  value: string;
  onChange: (effort: string) => void;
  disabled?: boolean;
}

// Both scope and session pickers consume the same provider-reported ladder.
// A missing ladder hides the picker instead of guessing model capabilities.

/** Reasoning-effort select for a chosen model; renders nothing when the model has no effort ladder. */
export class EffortPicker extends AntiHookComponent<IEffortPickerProps> {
  /**
   * Reports the chosen effort.
   *
   * @param e - the select change event
   */
  @bind
  protected onSelectChange(e: ChangeEvent<HTMLSelectElement>): void {
    this.props.onChange(e.target.value);
  }

  /** Renders the select. */
  public render() {
    const { provider, model, capabilities, value, disabled } = this.props;
    const levels = effortLevelsFor(provider, model, capabilities);
    if (levels === null) return null;

    const current = value
      ? clampEffort(provider, model, value, capabilities) ?? ""
      : defaultEffortFor(provider, model, capabilities);

    return (
      <select
        value={current}
        disabled={disabled}
        onChange={this.onSelectChange}
        title="Reasoning effort"
        aria-label="Reasoning effort"
        data-test-id="scoped-effort-picker"
        className="shrink-0 text-[11px] bg-canvas border border-surface rounded-lg px-1.5 py-1.5 outline-none focus:border-accent/50 text-text-subtle disabled:opacity-40 disabled:cursor-not-allowed"
      >
        {current === "" && <option value="">Provider default</option>}
        {levels.map(renderLevel)}
      </select>
    );
  }
}
