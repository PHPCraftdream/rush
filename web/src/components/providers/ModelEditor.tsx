import type { ChangeEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Plus, Trash2 } from "lucide-react";
import type { ModelDraft } from "./ModelDraft";
import { emptyModel } from "./emptyModel";

const inputCls = "w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50";

/** Props of one model row. */
interface IModelRowProps {
  index: number;
  model: ModelDraft;
  total: number;
  onUpdate: (index: number, field: keyof ModelDraft, value: string) => void;
  onRemove: (index: number) => void;
}

/** One custom model: its id, name, context window and prices. */
class ModelRow extends AntiHookComponent<IModelRowProps> {
  /** Removes this model. */
  @bind
  protected remove(): void {
    this.props.onRemove(this.props.index);
  }

  /**
   * Stores the typed model id.
   *
   * @param e - the input change event
   */
  @bind
  protected onIdChange(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onUpdate(this.props.index, "id", e.target.value);
  }

  /**
   * Stores the typed display name.
   *
   * @param e - the input change event
   */
  @bind
  protected onNameChange(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onUpdate(this.props.index, "name", e.target.value);
  }

  /**
   * Stores the typed context window.
   *
   * @param e - the input change event
   */
  @bind
  protected onContextChange(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onUpdate(this.props.index, "contextWindow", e.target.value);
  }

  /**
   * Stores the typed input price.
   *
   * @param e - the input change event
   */
  @bind
  protected onCostInChange(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onUpdate(this.props.index, "costPer1mIn", e.target.value);
  }

  /**
   * Stores the typed output price.
   *
   * @param e - the input change event
   */
  @bind
  protected onCostOutChange(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onUpdate(this.props.index, "costPer1mOut", e.target.value);
  }

  /** Renders the row. */
  public render() {
    const { index, model: m, total } = this.props;
    return (
      <div className="space-y-2 p-3 bg-base-overlay border border-surface rounded-xl">
        <div className="flex items-center justify-between">
          <span className="text-[11px] text-text-subtle font-medium">Model {index + 1}</span>
          {total > 1 && (
            <button onClick={this.remove} className="text-text-subtle hover:text-red transition-colors">
              <Trash2 size={12} />
            </button>
          )}
        </div>
        <div className="grid grid-cols-2 gap-2">
          <input value={m.id} onChange={this.onIdChange} placeholder="ID (e.g. qwen3:30b)" className={inputCls} />
          <input value={m.name} onChange={this.onNameChange} placeholder="Display name" className={inputCls} />
        </div>
        <div className="grid grid-cols-3 gap-2">
          <input
            value={m.contextWindow}
            onChange={this.onContextChange}
            placeholder="Context (tokens)"
            className={inputCls}
          />
          <input value={m.costPer1mIn} onChange={this.onCostInChange} placeholder="$/1M in" className={inputCls} />
          <input value={m.costPer1mOut} onChange={this.onCostOutChange} placeholder="$/1M out" className={inputCls} />
        </div>
      </div>
    );
  }
}

/** Props of the custom model editor. */
interface IModelEditorProps {
  models: ModelDraft[];
  onChange: (models: ModelDraft[]) => void;
}

/** List of custom model drafts with add/remove. */
export class ModelEditor extends AntiHookComponent<IModelEditorProps> {
  /** Appends a blank model. */
  @bind
  protected add(): void {
    const { models, onChange } = this.props;
    onChange([...models, emptyModel()]);
  }

  /**
   * Changes one field of one model.
   *
   * @param i - the model index
   * @param field - the field to change
   * @param value - the new text
   */
  @bind
  protected update(i: number, field: keyof ModelDraft, value: string): void {
    const { models, onChange } = this.props;
    const next = [...models];
    next[i] = { ...next[i], [field]: value };
    onChange(next);
  }

  /**
   * Removes one model.
   *
   * @param i - the model index
   */
  @bind
  protected remove(i: number): void {
    const { models, onChange } = this.props;
    onChange(models.filter((_, idx) => idx !== i));
  }

  /**
   * Renders one model row.
   *
   * @param m - the draft
   * @param i - its index
   */
  @bind
  protected renderModel(m: ModelDraft, i: number) {
    return (
      <ModelRow
        key={i}
        index={i}
        model={m}
        total={this.props.models.length}
        onUpdate={this.update}
        onRemove={this.remove}
      />
    );
  }

  /** Renders the rows and the Add button. */
  public render() {
    return (
      <div className="space-y-2">
        {this.props.models.map(this.renderModel)}
        <button
          onClick={this.add}
          className="flex items-center gap-1.5 text-xs text-accent hover:text-accent/80 transition-colors"
        >
          <Plus size={12} />
          Add model
        </button>
      </div>
    );
  }
}
