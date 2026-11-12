import type { ChangeEvent, MouseEvent as ReactMouseEvent } from "react";
import { createRef } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { BrainCircuit, Zap, Hammer, ShieldCheck, ChevronLeft, ChevronRight, Undo2 } from "lucide-react";
import {
  trackModelUsage,
  removeRecentModel,
  setSessionModel,
  setSessionRoleEffort,
  clearSessionModelSlot,
  type ModelRole,
} from "../store";
import type { Session } from "../types";
import { configCarburetor } from "../state/config";
import { effortLabel } from "../effort";
import { deriveModelSelection, type IModelSelection } from "./modelList/deriveModelSelection";
import type { ModelItem, ProviderGroup } from "./modelList/modelListTypes";
import { resolveRecentModels } from "./modelList/resolveRecentModels";
import { searchModels } from "./modelList/searchModels";

// Models and provider groups: the list builders live in ./modelList (one export per file).
export { buildModelList } from "./modelList/buildModelList";
export { buildProviderGroups } from "./modelList/buildProviderGroups";
export type { ModelItem, ProviderGroup } from "./modelList/modelListTypes";

// Per-role icon and toolbar title, keyed the same way as every other
// role-indexed table in this component (task #1061 generalized this
// selector from smart/fast-only to all four session model slots).
const ROLE_ICON: Record<ModelRole, typeof BrainCircuit> = {
  smart: BrainCircuit,
  fast: Zap,
  worker: Hammer,
  reviewer: ShieldCheck,
};
const ROLE_TITLE: Record<ModelRole, string> = {
  smart: "Smart (strong) model",
  fast: "Fast (cheap) model",
  worker: "Worker model (delegated sub-tasks)",
  reviewer: "Reviewer model (explicit review pass)",
};

// ── ModelRow ──────────────────────────────────────────────────────────────────

/** Props of one model row. */
interface IModelRowProps {
  model: ModelItem;
  isSelected: boolean;
  onSelect: (m: ModelItem) => void;
}

/** One selectable model of the dropdown. */
class ModelRow extends AntiHookComponent<IModelRowProps> {
  /** Reports this model as chosen. */
  @bind
  protected select(): void {
    this.props.onSelect(this.props.model);
  }

  /** Renders the row. */
  public render() {
    const { model, isSelected } = this.props;
    const disabled = !model.enabled;
    return (
      <button
        onClick={this.select}
        data-test-id={`model-item-${model.key}`}
        className={`w-full text-left px-3 py-2 transition-colors border-b border-surface/30 last:border-0 ${
          disabled ? "opacity-50 hover:bg-base-overlay" : isSelected ? "bg-accent/5 hover:bg-accent/8" : "hover:bg-base-overlay"
        }`}
      >
        <div
          className={`text-sm font-medium truncate ${
            isSelected ? "text-accent" : disabled ? "text-text-subtle" : "text-text"
          }`}
        >
          {model.name}
        </div>
      </button>
    );
  }
}

/** Props of one recent-model row. */
interface IRecentModelRowProps {
  model: ModelItem;
  isSelected: boolean;
  modelType: ModelRole;
  onSelect: (m: ModelItem) => void;
}

/** A recent model with its remove-from-recent button. */
class RecentModelRow extends AntiHookComponent<IRecentModelRowProps> {
  /**
   * Drops this model from the recent list.
   *
   * @param e - the click event
   */
  @bind
  protected removeRecent(e: ReactMouseEvent): void {
    e.stopPropagation();
    removeRecentModel(this.props.modelType, this.props.model.key);
  }

  /** Renders the row. */
  public render() {
    const { model, isSelected, onSelect } = this.props;
    return (
      <div className="flex items-center group/row">
        <div className="flex-1 min-w-0">
          <ModelRow model={model} isSelected={isSelected} onSelect={onSelect} />
        </div>
        <button
          onClick={this.removeRecent}
          title="Remove from recent"
          className="shrink-0 px-2 py-2 text-text-subtle hover:text-red opacity-0 group-hover/row:opacity-100 transition-opacity text-xs"
        >
          ✕
        </button>
      </div>
    );
  }
}

/** Props of one provider group section. */
interface IProviderGroupSectionProps {
  group: ProviderGroup;
  recentKeySet: ReadonlySet<string>;
  currentKey: string;
  onSelect: (m: ModelItem) => void;
}

/** A provider's models (minus the ones already shown under Recent). */
class ProviderGroupSection extends AntiHookComponent<IProviderGroupSectionProps> {
  /**
   * Renders one model row.
   *
   * @param m - the model
   */
  @bind
  protected renderModel(m: ModelItem) {
    const { currentKey, onSelect } = this.props;
    return <ModelRow key={m.key} model={m} isSelected={m.key === currentKey} onSelect={onSelect} />;
  }

  /**
   * Whether a model is not in the recent list.
   *
   * @param m - the model
   */
  @bind
  protected isNotRecent(m: ModelItem): boolean {
    return !this.props.recentKeySet.has(m.key);
  }

  /** Renders the section, or nothing when every model is under Recent. */
  public render() {
    const { group } = this.props;
    const groupModels = group.models.filter(this.isNotRecent);
    if (groupModels.length === 0) return null;
    return (
      <div className="py-1">
        <div className="px-3 py-1.5 flex items-center gap-2">
          <span className="text-[10px] font-bold text-text-muted uppercase tracking-wider">{group.name}</span>
        </div>
        {groupModels.map(this.renderModel)}
      </div>
    );
  }
}

// ── Effort control ────────────────────────────────────────────────────────────

/** Props of the effort control. */
interface IEffortControlProps {
  modelType: ModelRole;
  currentEffort: string;
  onCycle: (direction: 1 | -1) => void;
}

/** The reasoning-effort stepper shown inside the selector button. */
class EffortControl extends AntiHookComponent<IEffortControlProps> {
  /**
   * Keeps clicks on the stepper from toggling the dropdown.
   *
   * @param e - the click event
   */
  @bind
  protected stopPropagation(e: ReactMouseEvent): void {
    e.stopPropagation();
  }

  /** Steps the effort down. */
  @bind
  protected decrease(): void {
    this.props.onCycle(-1);
  }

  /** Steps the effort up. */
  @bind
  protected increase(): void {
    this.props.onCycle(1);
  }

  /** Renders the stepper. */
  public render() {
    const { modelType, currentEffort } = this.props;
    return (
      <div
        className="flex items-center gap-0.5 shrink-0 ml-1"
        onClick={this.stopPropagation}
        data-test-id={`reasoning-effort-${modelType}`}
      >
        <button
          onClick={this.decrease}
          className="p-0.5 rounded hover:bg-base-subtle text-text-subtle hover:text-text transition-colors"
          title={`Reasoning effort: ${currentEffort} (click to decrease)`}
          data-test-id={`reasoning-effort-${modelType}-decrease`}
        >
          <ChevronLeft size={12} strokeWidth={2.5} />
        </button>
        <span
          className="px-1 py-0.5 rounded bg-base-subtle text-text font-mono text-[10px] min-w-[16px] text-center"
          title={`Reasoning effort: ${currentEffort}`}
          data-test-id={`reasoning-effort-${modelType}-label`}
        >
          {currentEffort ? effortLabel(currentEffort) : "AUTO"}
        </span>
        <button
          onClick={this.increase}
          className="p-0.5 rounded hover:bg-base-subtle text-text-subtle hover:text-text transition-colors"
          title={`Reasoning effort: ${currentEffort} (click to increase)`}
          data-test-id={`reasoning-effort-${modelType}-increase`}
        >
          <ChevronRight size={12} strokeWidth={2.5} />
        </button>
      </div>
    );
  }
}

// ── Dropdown ──────────────────────────────────────────────────────────────────

/** Props of the model dropdown. */
interface IModelDropdownProps {
  modelType: ModelRole;
  left: number;
  bottom: number;
  search: string;
  onSearchChange: (search: string) => void;
  selection: IModelSelection;
  recentKeys: readonly string[];
  onSelect: (m: ModelItem) => void;
  onInherit: () => void;
}

/** The searchable model list opened from the selector button. */
class ModelDropdown extends AntiHookComponent<IModelDropdownProps> {
  /**
   * Stores the typed search.
   *
   * @param e - the input change event
   */
  @bind
  protected onSearchInput(e: ChangeEvent<HTMLInputElement>): void {
    this.props.onSearchChange(e.target.value);
  }

  /**
   * Renders one model row.
   *
   * @param m - the model
   */
  @bind
  protected renderModel(m: ModelItem) {
    const { selection, onSelect } = this.props;
    return <ModelRow key={m.key} model={m} isSelected={m.key === selection.currentKey} onSelect={onSelect} />;
  }

  /**
   * Renders one recent model.
   *
   * @param m - the model
   */
  @bind
  protected renderRecent(m: ModelItem) {
    const { selection, modelType, onSelect } = this.props;
    return (
      <RecentModelRow
        key={m.key}
        model={m}
        isSelected={m.key === selection.currentKey}
        modelType={modelType}
        onSelect={onSelect}
      />
    );
  }

  /**
   * Renders one provider's section.
   *
   * @param group - the provider group
   */
  @bind
  protected renderGroup(group: ProviderGroup) {
    const { selection, recentKeys, onSelect } = this.props;
    return (
      <ProviderGroupSection
        key={group.id}
        group={group}
        recentKeySet={new Set(recentKeys)}
        currentKey={selection.currentKey}
        onSelect={onSelect}
      />
    );
  }

  /** Renders the search results. */
  protected renderSearchResults() {
    const searchResults = searchModels(this.props.selection.allModels, this.props.search.toLowerCase());
    if (searchResults.length === 0) {
      return <p className="text-text-subtle text-sm text-center py-4">No models found</p>;
    }
    return searchResults.map(this.renderModel);
  }

  /** Renders the inherit row, the recent models and the provider groups. */
  protected renderBrowse() {
    const { modelType, selection, recentKeys, onInherit } = this.props;
    const recentModels = resolveRecentModels(recentKeys, selection.allModels);
    return (
      <>
        {selection.hasSessionOverride && (
          <div className="py-1 border-b border-surface/40">
            <button
              onClick={onInherit}
              data-test-id={`model-inherit-${modelType}`}
              className="w-full text-left px-3 py-2 flex items-center gap-2 text-sm text-text-subtle hover:bg-base-overlay hover:text-text transition-colors"
              title="Clear this session's override — follow the folder/system default"
            >
              <Undo2 size={13} className="shrink-0" />
              Inherit (folder/system default)
            </button>
          </div>
        )}
        {recentModels.length > 0 && (
          <div className="py-1">
            <div className="px-3 py-1.5 text-[10px] font-bold text-text-muted uppercase tracking-wider">Recent</div>
            {recentModels.map(this.renderRecent)}
            <div className="h-px bg-surface/40 my-1" />
          </div>
        )}
        {selection.providerGroups.map(this.renderGroup)}
      </>
    );
  }

  /** Renders the dropdown. */
  public render() {
    const { left, bottom, search } = this.props;
    return (
      <div
        data-test-id="model-dropdown"
        style={{ position: "fixed", left, bottom, width: 520, zIndex: 9999 }}
        className="bg-canvas border border-surface rounded-xl shadow-xl overflow-hidden"
      >
        <div className="max-h-[480px] overflow-y-auto">
          {search ? this.renderSearchResults() : this.renderBrowse()}
        </div>
        <div className="p-2.5 border-t border-surface/40">
          <input
            autoFocus
            value={search}
            onChange={this.onSearchInput}
            placeholder="Search models…"
            className="w-full bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 text-sm text-text outline-none focus:border-accent transition-colors placeholder:text-text-subtle"
          />
        </div>
      </div>
    );
  }
}

// ── ModelSelector ─────────────────────────────────────────────────────────────

/** Props of the model selector. */
interface IModelSelectorProps {
  session: Session | null;
  modelType: ModelRole;
}

/** Local state of the model selector. */
interface IModelSelectorState {
  open: boolean;
  search: string;
  dropdownPos: { left: number; bottom: number };
}

/** Per-role model picker with an inline reasoning-effort stepper. */
export class ModelSelector extends AntiHookComponent<IModelSelectorProps, IModelSelectorState> {
  public state: IModelSelectorState = { open: false, search: "", dropdownPos: { left: 0, bottom: 0 } };

  private readonly rootRef = createRef<HTMLDivElement>();

  private readonly btnRef = createRef<HTMLButtonElement>();

  // The selection derived by the latest render; effects and the effort
  // stepper act on what the operator is looking at.
  private selection: IModelSelection | null = null;

  /** Corrects an invalid stored effort, and tracks the open dropdown's listeners. */
  protected useEffects(): void {
    super.useEffects();
    const { session, modelType } = this.props;
    const sel = this.selection;
    this.useEffect(
      "fixEffort",
      this.fixEffort,
      [session?.ID, modelType, sel?.showEffortPicker, sel?.effortValid, sel?.currentEffort],
    );
    this.useEffect("dropdownListeners", this.bindDropdownListeners, [this.state.open]);
  }

  /** Rewrites the session's effort when it is not valid for the shown model. */
  @bind
  protected fixEffort(): void {
    const { session, modelType } = this.props;
    const sel = this.selection;
    if (!session || !sel || !sel.showEffortPicker) return;
    if (sel.effortValid) return;
    setSessionRoleEffort(session.ID, modelType, sel.currentEffort);
  }

  /** While the dropdown is open: tracks its position and closes it on an outside click. */
  @bind
  protected bindDropdownListeners(): (() => void) | undefined {
    if (!this.state.open) return undefined;
    this.updatePos();
    window.addEventListener("resize", this.updatePos);
    window.addEventListener("scroll", this.updatePos, true);
    document.addEventListener("mousedown", this.onDocMouseDown);
    return this.unbindDropdownListeners;
  }

  /** Detaches the dropdown listeners. */
  @bind
  protected unbindDropdownListeners(): void {
    document.removeEventListener("mousedown", this.onDocMouseDown);
    window.removeEventListener("resize", this.updatePos);
    window.removeEventListener("scroll", this.updatePos, true);
  }

  /** Anchors the dropdown above the selector button, kept inside the viewport. */
  @bind
  protected updatePos(): void {
    if (!this.btnRef.current) return;
    const r = this.btnRef.current.getBoundingClientRect();
    const width = 520;
    const margin = 8;
    const left = Math.min(r.left, window.innerWidth - width - margin);
    this.setState({ dropdownPos: { left: Math.max(margin, left), bottom: window.innerHeight - r.top + 8 } });
  }

  /**
   * Closes the dropdown on a mousedown outside the selector.
   *
   * @param e - the mousedown event
   */
  @bind
  protected onDocMouseDown(e: MouseEvent): void {
    if (this.rootRef.current && !this.rootRef.current.contains(e.target as Node)) {
      this.setState({ open: false });
    }
  }

  /**
   * Steps the session's effort through the model's ladder, wrapping around.
   *
   * @param direction - 1 up, -1 down
   */
  @bind
  protected cycleEffort(direction: 1 | -1): void {
    const { session, modelType } = this.props;
    const sel = this.selection;
    if (!session || !sel || !sel.showEffortPicker) return;
    const { effortLevels, currentEffort } = sel;
    const idx = effortLevels.indexOf(currentEffort);
    const newIdx = idx < 0
      ? (direction === 1 ? 0 : effortLevels.length - 1)
      : (idx + direction + effortLevels.length) % effortLevels.length;
    setSessionRoleEffort(session.ID, modelType, effortLevels[newIdx]);
  }

  /** Opens or closes the dropdown, clearing the search. */
  @bind
  protected toggleOpen(): void {
    this.setState({ open: !this.state.open, search: "" });
  }

  /**
   * Stores the typed search.
   *
   * @param search - the query
   */
  @bind
  protected onSearchChange(search: string): void {
    this.setState({ search });
  }

  /**
   * Switches this role's model for the session.
   *
   * @param m - the chosen model
   */
  @bind
  protected onSelect(m: ModelItem): void {
    const { session, modelType } = this.props;
    if (!m.enabled) return; // CLI providers can't be selected without being enabled
    if (session) {
      // setSessionModel touches ONLY this role's slot (task #461/#1061) —
      // filling other slots in from their current/default value here, like
      // this used to do for smart/fast, would re-write them on every switch
      // and freeze them against later folder/system default changes.
      setSessionModel(session.ID, modelType, m.key);
      trackModelUsage(modelType, m.key);
      this.setState({ open: false });
    }
  }

  // Clears this session's override, falling back to inheriting the
  // folder/system default (task #467). Only meaningful — and only shown —
  // when the session currently HAS an explicit override for this slot.
  /** Clears this role's session override. */
  @bind
  protected onInherit(): void {
    const { session, modelType } = this.props;
    if (!session) return;
    clearSessionModelSlot(session.ID, modelType);
    this.setState({ open: false });
  }

  /** Renders the selector button and, when open, the dropdown. */
  public render() {
    const { session, modelType } = this.props;
    const { open, search, dropdownPos } = this.state;
    const { config, recent } = this.useCarburetor(configCarburetor);
    const recentKeys = recent[modelType];
    const selection = deriveModelSelection(config, session, modelType);
    this.selection = selection;
    const { allModels, displayName, showEffortPicker, currentEffort } = selection;

    const Icon = ROLE_ICON[modelType];
    const title = ROLE_TITLE[modelType];

    if (!session || allModels.length === 0) {
      return (
        <span
          className="flex items-center gap-1.5 text-xs text-text-subtle bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5"
          title={title}
        >
          <Icon size={12} />
          {displayName}
        </span>
      );
    }

    return (
      <div ref={this.rootRef} className="relative">
        <button
          ref={this.btnRef}
          onClick={this.toggleOpen}
          className="flex items-center gap-1.5 text-xs text-text bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 hover:border-accent/50 hover:bg-base-subtle transition-colors"
          title={title}
          data-test-id={`model-selector-${modelType}`}
        >
          <Icon size={12} className="shrink-0" />
          <span className="font-medium truncate max-w-[180px]">{displayName}</span>
          {showEffortPicker && (
            <EffortControl modelType={modelType} currentEffort={currentEffort} onCycle={this.cycleEffort} />
          )}
          <span className="text-text-subtle ml-auto">{open ? "▴" : "▾"}</span>
        </button>
        {open && (
          <ModelDropdown
            modelType={modelType}
            left={dropdownPos.left}
            bottom={dropdownPos.bottom}
            search={search}
            onSearchChange={this.onSearchChange}
            selection={selection}
            recentKeys={recentKeys}
            onSelect={this.onSelect}
            onInherit={this.onInherit}
          />
        )}
      </div>
    );
  }
}
