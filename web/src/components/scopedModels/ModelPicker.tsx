import type { ChangeEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { ModelItem } from "../modelList/modelListTypes";

// ── Model picker — plain <select>, grouped by provider ───────────────────────
// A settings modal doesn't need ModelSelector's rich search dropdown; a
// native grouped <select> is far less code and perfectly adequate here.

/**
 * Orders provider groups by name.
 *
 * @param a - first [name, models] pair
 * @param b - second [name, models] pair
 */
const byProviderName = ([a]: [string, ModelItem[]], [b]: [string, ModelItem[]]): number => a.localeCompare(b);

/**
 * Renders one model option.
 *
 * @param m - the model
 */
const renderModelOption = (m: ModelItem) => <option key={m.key} value={m.key}>{m.name}</option>;

/**
 * Groups models by provider name, sorted by name.
 *
 * @param models - the catalog
 */
function groupByProvider(models: readonly ModelItem[]): [string, ModelItem[]][] {
  const byProvider = new Map<string, ModelItem[]>();
  for (const m of models) {
    const list = byProvider.get(m.providerName) ?? [];
    list.push(m);
    byProvider.set(m.providerName, list);
  }
  return [...byProvider.entries()].sort(byProviderName);
}

/**
 * Renders one provider's option group.
 *
 * @param entry - the [provider name, models] pair
 * @param entry.0 - the provider name
 * @param entry.1 - its models
 */
const renderGroup = ([providerName, items]: [string, ModelItem[]]) => (
  <optgroup key={providerName} label={providerName}>
    {items.map(renderModelOption)}
  </optgroup>
);

/** Props of the model picker. */
interface IModelPickerProps {
  models: ModelItem[];
  value: string; // "" or "provider:::model"
  onChange: (provider: string, model: string) => void;
  disabled?: boolean;
}

/** Native grouped select of the catalog's models. */
export class ModelPicker extends AntiHookComponent<IModelPickerProps> {
  /**
   * Reports the chosen provider and model.
   *
   * @param e - the select change event
   */
  @bind
  protected onSelectChange(e: ChangeEvent<HTMLSelectElement>): void {
    const key = e.target.value;
    if (!key) return;
    const idx = key.indexOf(":::");
    if (idx === -1) return;
    this.props.onChange(key.slice(0, idx), key.slice(idx + 3));
  }

  /** Renders the select. */
  public render() {
    const { models, value, disabled } = this.props;
    return (
      <select
        value={value}
        disabled={disabled}
        onChange={this.onSelectChange}
        className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text disabled:opacity-40 disabled:cursor-not-allowed"
      >
        <option value="">Choose a model…</option>
        {groupByProvider(models).map(renderGroup)}
      </select>
    );
  }
}
