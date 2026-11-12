import type { ChangeEvent, KeyboardEvent, MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { addContextPath, removeContextPath, addSkillsPath, removeSkillsPath, setActiveSession } from "../store";
import { wsRequest } from "../ws";
import { X, Plus, Trash2, RefreshCw, FolderOpen, Loader2 } from "lucide-react";
import type { SkillInfo, SkillsSnapshot } from "../types";
import { configCarburetor } from "../state/config";
import type { IConfigState } from "../state/ConfigCarburetor";

/** Shared empty path list for a missing config. */
const NO_PATHS: TReadonly<string[]> = [];

/**
 * Picks the context paths out of the config state.
 *
 * @param state - the config state
 */
const selectContextPaths = (state: TReadonly<IConfigState>) => state.config?.contextPaths ?? NO_PATHS;

/**
 * Picks the skills paths out of the config state.
 *
 * @param state - the config state
 */
const selectSkillsPaths = (state: TReadonly<IConfigState>) => state.config?.skillsPaths ?? NO_PATHS;

/**
 * The text of a thrown value.
 *
 * @param e - the thrown value
 */
const errorText = (e: unknown): string => (e instanceof Error ? e.message : String(e));

// ── Path list (reused for context_paths and skills_paths) ─────────────────────

/** Props of one path row. */
interface IPathItemProps {
  path: string;
  index: number;
  testId: string;
  onRemove: (path: string) => void;
}

/** One configured path with its Remove button. */
class PathItem extends AntiHookComponent<IPathItemProps> {
  /** Removes this path. */
  @bind
  protected remove(): void {
    this.props.onRemove(this.props.path);
  }

  /** Renders the row. */
  public render() {
    const { path, index: idx, testId } = this.props;
    return (
      <li
        className="flex items-center gap-2 bg-base-overlay border border-surface rounded-lg px-3 py-1.5"
        data-test-id={`${testId}-item-${idx}`}
      >
        <span className="flex-1 text-xs font-mono text-text truncate">{path}</span>
        <button
          onClick={this.remove}
          className="shrink-0 text-text-subtle hover:text-red transition-colors"
          title="Remove"
          data-test-id={`${testId}-remove-${idx}`}
        >
          <Trash2 size={13} />
        </button>
      </li>
    );
  }
}

/** Props of the path list. */
interface IPathListProps {
  paths: readonly string[];
  onAdd: (p: string) => void;
  onRemove: (p: string) => void;
  placeholder: string;
  testId: string;
}

/** Local state of the path list. */
interface IPathListState {
  draft: string;
}

/** Editable list of paths with an input to add more. */
class PathList extends AntiHookComponent<IPathListProps, IPathListState> {
  public state: IPathListState = { draft: "" };

  /** Adds the trimmed draft and clears the input; ignores a blank draft. */
  @bind
  protected add(): void {
    const v = this.state.draft.trim();
    if (!v) return;
    this.props.onAdd(v);
    this.setState({ draft: "" });
  }

  /**
   * Enter adds the draft.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    if (e.key === "Enter") this.add();
  }

  /**
   * Stores the typed draft.
   *
   * @param e - the input change event
   */
  @bind
  protected onDraftChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ draft: e.target.value });
  }

  /**
   * Renders one path row.
   *
   * @param p - the path
   * @param idx - its index
   */
  @bind
  protected renderPath(p: string, idx: number) {
    return <PathItem key={p} path={p} index={idx} testId={this.props.testId} onRemove={this.props.onRemove} />;
  }

  /** Renders the list and the add input. */
  public render() {
    const { paths, placeholder, testId } = this.props;
    const { draft } = this.state;
    return (
      <div className="space-y-2" data-test-id={testId}>
        {paths.length > 0 ? (
          <ul className="space-y-1">
            {paths.map(this.renderPath)}
          </ul>
        ) : (
          <p className="text-xs text-text-muted italic">No paths configured</p>
        )}
        <div className="flex gap-2">
          <input
            value={draft}
            onChange={this.onDraftChange}
            onKeyDown={this.onKey}
            placeholder={placeholder}
            className="flex-1 text-xs font-mono text-text bg-canvas border border-surface rounded-lg px-3 py-1.5 outline-none focus:border-accent/50 transition-colors placeholder:text-text-muted/50"
            data-test-id={`${testId}-input`}
          />
          <button
            onClick={this.add}
            disabled={!draft.trim()}
            className="px-3 py-1.5 text-xs font-medium bg-accent-fill text-white/90 rounded-lg hover:opacity-90 disabled:opacity-40 flex items-center gap-1"
            data-test-id={`${testId}-add`}
          >
            <Plus size={12} />
            Add
          </button>
        </div>
      </div>
    );
  }
}

// ── Skills section ────────────────────────────────────────────────────────────

/**
 * Renders one discovered skill.
 *
 * @param s - the skill
 * @param idx - its index
 */
const renderSkill = (s: SkillInfo, idx: number) => (
  <li
    key={s.path}
    className="bg-base-overlay border border-surface rounded-lg px-3 py-2"
    data-test-id={`settings-skill-item-${idx}`}
  >
    <div className="text-xs font-semibold text-text">{s.name}</div>
    {s.description && <div className="text-[11px] text-text-subtle mt-0.5 line-clamp-2">{s.description}</div>}
    <div className="text-[10px] font-mono text-text-muted mt-1 truncate">{s.path}</div>
  </li>
);

/** Local state of the skills section. */
interface ISkillsSectionState {
  snapshot: SkillsSnapshot | null;
  loading: boolean;
  error: string | null;
}

/** Discovered-skills list with a refresh button. */
class SkillsSection extends AntiHookComponent<{}, ISkillsSectionState> {
  public state: ISkillsSectionState = { snapshot: null, loading: false, error: null };

  // refresh()'s continuation checks this after its await so a late reply
  // cannot setState on an unmounted section (wsRequest's listeners detach
  // themselves on settle).
  private disposed = false;

  /** Tracks mount state and loads the skills once on mount. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("lifecycle", this.markMounted, []);
    this.useEffect("autoRefresh", this.autoRefresh, []);
  }

  /** Clears the disposed flag; returns the cleanup that sets it. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Marks the section as unmounted. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  /** Loads the skills on mount. */
  @bind
  protected autoRefresh(): void {
    void this.refresh();
  }

  /** Requests the discovered skills. */
  @bind
  protected async refresh(): Promise<void> {
    this.setState({ loading: true, error: null });
    try {
      const reply = await wsRequest<SkillsSnapshot>("get_skills", {});
      if (this.disposed) return;
      this.setState({ loading: false, snapshot: reply.payload as SkillsSnapshot });
    } catch (e) {
      if (this.disposed) return;
      this.setState({ loading: false, error: errorText(e) });
    }
  }

  /** Click handler of the Refresh button. */
  @bind
  protected onRefreshClick(): void {
    void this.refresh();
  }

  /** Renders the section. */
  public render() {
    const { snapshot, loading, error } = this.state;
    return (
      <div className="space-y-3" data-test-id="settings-skills-section">
        <div className="flex items-center justify-between">
          <span className="text-xs font-semibold text-text-subtle uppercase tracking-wider">Discovered Skills</span>
          <button
            onClick={this.onRefreshClick}
            disabled={loading}
            title="Refresh skills"
            className="text-text-subtle hover:text-accent transition-colors disabled:opacity-40"
            data-test-id="settings-skills-refresh"
          >
            <RefreshCw size={13} className={loading ? "animate-spin" : ""} />
          </button>
        </div>
        {error && (
          <p className="text-xs text-red" data-test-id="settings-skills-error">{error}</p>
        )}
        {loading ? (
          <p className="text-xs text-text-subtle">Scanning…</p>
        ) : snapshot && snapshot.skills.length > 0 ? (
          <ul className="space-y-1.5">
            {snapshot.skills.map(renderSkill)}
          </ul>
        ) : (
          <p className="text-xs text-text-muted italic">No skills found in configured paths</p>
        )}
      </div>
    );
  }
}

// ── Main Settings Modal ───────────────────────────────────────────────────────

/** Props of the settings dialog. */
interface ISettingsModalProps {
  onClose: () => void;
}

/** Local state of the settings dialog. */
interface ISettingsModalState {
  initBusy: boolean;
  initDone: boolean;
  initError: string | null;
}

/** Settings dialog: project initialization, context paths and skills paths. */
export class SettingsModal extends AntiHookComponent<ISettingsModalProps, ISettingsModalState> {
  public state: ISettingsModalState = { initBusy: false, initDone: false, initError: null };

  /** Detached snapshot of the context paths, safe to hand to the list. */
  private readonly contextPathsView = this.connectSelection(configCarburetor, selectContextPaths);

  /** Detached snapshot of the skills paths, safe to hand to the list. */
  private readonly skillsPathsView = this.connectSelection(configCarburetor, selectSkillsPaths);

  // handleInitialize's continuation checks this after its await so a late
  // initialize_project reply cannot setActiveSession (a global-atom
  // navigation write) or onClose() after the operator dismissed Settings
  // and moved on to another session.
  private disposed = false;

  /** Tracks mount state and closes on Escape while mounted. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("lifecycle", this.markMounted, []);
    this.useEffect("escapeKey", this.bindEscape, [this.props.onClose]);
  }

  /** Clears the disposed flag; returns the cleanup that sets it. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Marks the dialog as dismissed. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
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
  protected onKey(e: globalThis.KeyboardEvent): void {
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

  /** Asks the server to analyze the project, then opens the resulting session. */
  @bind
  protected async handleInitialize(): Promise<void> {
    this.setState({ initError: null, initBusy: true });
    try {
      const reply = await wsRequest<{ sessionID?: string }>("initialize_project", {});
      if (this.disposed) return;
      this.setState({ initBusy: false, initDone: true });
      // Navigate to the new session
      if (reply.payload?.sessionID) {
        setActiveSession(reply.payload.sessionID);
        this.props.onClose();
      }
    } catch (e) {
      if (this.disposed) return;
      this.setState({ initBusy: false, initError: errorText(e) });
    }
  }

  /** Click handler of the Initialize button. */
  @bind
  protected onInitializeClick(): void {
    void this.handleInitialize();
  }

  /** Renders the dialog. */
  public render() {
    const { onClose } = this.props;
    const { initBusy, initDone, initError } = this.state;
    const config = this.useCarburetor(configCarburetor).config;
    const contextPaths = this.contextPathsView();
    const skillsPaths = this.skillsPathsView();

    return (
      <div
        className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
        onClick={onClose}
        data-test-id="settings-modal-overlay"
      >
        <div
          className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[85vh] chat-font"
          onClick={this.stopPropagation}
          data-test-id="settings-modal"
        >
          {/* Header */}
          <div
            className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0"
            data-test-id="settings-modal-header"
          >
            <div>
              <h2 className="text-base font-semibold text-text">Settings</h2>
              <p className="text-xs text-text-subtle mt-0.5">Context and skills configuration</p>
            </div>
            <button
              onClick={onClose}
              className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay"
              data-test-id="settings-modal-close"
            >
              <X size={16} />
            </button>
          </div>

          {/* Body */}
          <div className="flex-1 overflow-y-auto divide-y divide-surface" data-test-id="settings-modal-body">

            {/* Initialize project */}
            <section className="px-5 py-4 space-y-3" data-test-id="settings-section-init">
              <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-wider">
                Project Initialization
              </h3>
              <p className="text-xs text-text-subtle leading-relaxed">
                Analyze the codebase and create or update{" "}
                <span className="font-mono text-text">{config?.initializeAs ?? "AGENTS.md"}</span>{" "}
                to help the agent work more effectively.
              </p>
              <button
                onClick={this.onInitializeClick}
                disabled={initBusy || initDone}
                className="flex items-center gap-2 px-4 py-2 text-sm font-medium bg-accent-fill text-white/90 rounded-xl hover:opacity-90 disabled:opacity-40 transition-all"
                data-test-id="settings-init-button"
              >
                {initBusy ? (
                  <><Loader2 size={14} className="animate-spin" /> Initializing…</>
                ) : initDone ? (
                  <>Done!</>
                ) : (
                  <><FolderOpen size={14} /> Initialize Project</>
                )}
              </button>
              {initError && (
                <p className="text-xs text-red" data-test-id="settings-init-error">{initError}</p>
              )}
            </section>

            {/* Context paths */}
            <section className="px-5 py-4 space-y-3" data-test-id="settings-section-context">
              <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-wider">Context Paths</h3>
              <p className="text-xs text-text-subtle leading-relaxed">
                Additional files the agent reads for project context
                (beyond defaults like AGENTS.md, CLAUDE.md).
              </p>
              <PathList
                paths={contextPaths}
                onAdd={addContextPath}
                onRemove={removeContextPath}
                placeholder="e.g. docs/architecture.md"
                testId="settings-context-paths"
              />
            </section>

            {/* Skills paths */}
            <section className="px-5 py-4 space-y-3" data-test-id="settings-section-skills">
              <h3 className="text-xs font-semibold text-text-subtle uppercase tracking-wider">Agent Skills Paths</h3>
              <p className="text-xs text-text-subtle leading-relaxed">
                Directories containing Agent Skills (folders with SKILL.md files).
              </p>
              <PathList
                paths={skillsPaths}
                onAdd={addSkillsPath}
                onRemove={removeSkillsPath}
                placeholder="e.g. ./project-skills"
                testId="settings-skills-paths"
              />
              <SkillsSection />
            </section>
          </div>

          {/* Footer */}
          <div className="px-5 py-3 border-t border-surface bg-base-overlay/50" data-test-id="settings-modal-footer">
            <p className="text-xs text-text-subtle text-center">
              {config?.version || "Development build"}
            </p>
          </div>
        </div>
      </div>
    );
  }
}
