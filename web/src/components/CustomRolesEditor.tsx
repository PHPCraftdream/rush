import type { ChangeEvent, FormEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type {
  CustomRole,
  CustomRoleScope,
  CustomRolesState,
  SetCustomRoleRequest,
  WSMessage,
} from "../types";
import { customRoleAt, roleToolsMode, roleToolsPayload, type RoleToolsMode } from "../customRoleTypes";
import { effortLevelsFor } from "../effort";
import { ws, wsRequest, WSRequestError } from "../ws";
import type { ModelItem } from "./ModelSelector";

const fieldClass = "w-full rounded-lg border border-surface bg-canvas px-2.5 py-1.5 text-xs text-text disabled:opacity-40";

/** Counter behind the per-instance id prefix of the datalists. */
let editorSeq = 0;

/** Returns a fresh id prefix, unique per editor instance. */
function nextEditorID(): string {
  editorSeq += 1;
  return `custom-roles-${editorSeq}`;
}

/**
 * The text of a thrown value.
 *
 * @param failure - the thrown value
 */
function failureMessage(failure: unknown): string {
  return failure instanceof Error ? failure.message : String(failure);
}

/**
 * Every role name defined at any scope, sorted.
 *
 * @param roles - the loaded roles, if any
 */
function roleNames(roles: CustomRolesState | null): string[] {
  return [
    ...new Set([
      ...Object.keys(roles?.global ?? {}),
      ...Object.keys(roles?.workspace ?? {}),
      ...Object.keys(roles?.effective ?? {}),
    ]),
  ].sort();
}

/**
 * Where a role is defined, for its list label.
 *
 * @param roles - the loaded roles
 * @param roleName - the role
 */
function roleSources(roles: CustomRolesState | null, roleName: string): string {
  const sources = [
    customRoleAt(roles?.global, roleName) && "global",
    customRoleAt(roles?.workspace, roleName) && "workspace",
  ];
  return sources.filter(Boolean).join(", ") || "inherited";
}

/**
 * The catalog entry of a provider/model pair, if cached.
 *
 * @param models - the catalog
 * @param provider - provider id
 * @param model - model id
 */
function findModel(models: readonly ModelItem[], provider: string, model: string): ModelItem | undefined {
  return models.find((item) => item.providerID === provider && item.modelID === model);
}

/**
 * The provider id of a catalog entry.
 *
 * @param item - the model
 */
const providerOf = (item: ModelItem): string => item.providerID;

/**
 * The sorted distinct provider ids of the catalog.
 *
 * @param models - the catalog
 */
function providerIDs(models: readonly ModelItem[]): string[] {
  return [...new Set(models.map(providerOf))].sort();
}

/**
 * The catalog entries of one provider.
 *
 * @param models - the catalog
 * @param provider - provider id
 */
function modelsOfProvider(models: readonly ModelItem[], provider: string): ModelItem[] {
  return models.filter((item) => item.providerID === provider);
}

/**
 * Renders one datalist option of a provider.
 *
 * @param value - the provider id
 */
const renderProviderOption = (value: string) => <option key={value} value={value}>{value}</option>;

/**
 * Renders one datalist option of a model.
 *
 * @param item - the model
 */
const renderModelOption = (item: ModelItem) => <option key={item.key} value={item.modelID}>{item.name}</option>;

/**
 * Renders one reasoning-effort option.
 *
 * @param level - the effort level
 */
const renderLevelOption = (level: string) => <option key={level} value={level}>{level}</option>;

/** Props of one role list row. */
interface IRoleListItemProps {
  roleName: string;
  sources: string;
  effective: CustomRole | undefined;
  busy: boolean;
  onEdit: (roleName: string) => void;
}

/** One role of the list with its Edit button. */
class RoleListItem extends AntiHookComponent<IRoleListItemProps> {
  /** Opens this role in the form. */
  @bind
  protected edit(): void {
    this.props.onEdit(this.props.roleName);
  }

  /** Renders the row. */
  public render() {
    const { roleName, sources, effective, busy } = this.props;
    return (
      <li className="flex items-center justify-between gap-2 text-xs">
        <span className="min-w-0 break-all text-text">
          {roleName} <span className="text-text-subtle">({sources}) {effective?.provider}/{effective?.model}</span>
        </span>
        <button
          type="button"
          disabled={busy}
          onClick={this.edit}
          aria-label={`Edit role ${roleName}`}
          className="text-accent disabled:opacity-40"
        >
          Edit
        </button>
      </li>
    );
  }
}

/** Props of the custom-roles editor. */
interface ICustomRolesEditorProps {
  models: ModelItem[];
}

/** Local state of the custom-roles editor. */
interface ICustomRolesEditorState {
  roles: CustomRolesState | null;
  scope: CustomRoleScope;
  editingName: string | null;
  name: string;
  provider: string;
  model: string;
  effort: string;
  think: boolean;
  toolsMode: RoleToolsMode;
  patterns: string;
  toolsChanged: boolean;
  hadScopedDefinition: boolean;
  busy: boolean;
  error: string;
  notice: string;
}

/** Settings section for defining named model profiles (custom roles) at global or workspace scope. */
export class CustomRolesEditor extends AntiHookComponent<ICustomRolesEditorProps, ICustomRolesEditorState> {
  public state: ICustomRolesEditorState = {
    roles: null,
    scope: "global",
    editingName: null,
    name: "",
    provider: "",
    model: "",
    effort: "",
    think: false,
    toolsMode: "inherit",
    patterns: "[]",
    toolsChanged: false,
    hadScopedDefinition: false,
    busy: false,
    error: "",
    notice: "",
  };

  private readonly id = nextEditorID();

  private mounted = false;

  private requestGeneration: symbol | null = null;

  private detachers: (() => void)[] = [];

  /** Subscribes to role pushes and reconnects, and loads the roles once. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("wsSubscriptions", this.subscribe, []);
  }

  /** Starts listening and schedules the initial load; returns the teardown. */
  @bind
  protected subscribe(): () => void {
    this.mounted = true;
    this.detachers = [ws.on("custom_roles", this.onRolesPush), ws.on("_connected", this.onReconnect)];
    queueMicrotask(this.refreshIfMounted);
    return this.unsubscribe;
  }

  /** Stops listening and drops any in-flight reply. */
  @bind
  protected unsubscribe(): void {
    this.mounted = false;
    this.requestGeneration = null;
    for (const detach of this.detachers) detach();
    this.detachers = [];
  }

  /**
   * Applies a server-pushed roles snapshot (replies carrying a request id are ignored).
   *
   * @param message - the pushed frame
   */
  @bind
  protected onRolesPush(message: WSMessage): void {
    if (message.id || !message.payload) return;
    this.requestGeneration = null;
    this.setState({ roles: message.payload as CustomRolesState });
  }

  /** Reloads the roles after a reconnect. */
  @bind
  protected onReconnect(): void {
    void this.refresh();
  }

  /** Reloads the roles when the first microtask finds the editor still mounted. */
  @bind
  protected refreshIfMounted(): void {
    if (this.mounted) void this.refresh();
  }

  /** Requests the roles; only the newest request's reply is applied. */
  @bind
  protected async refresh(): Promise<void> {
    const generation = Symbol("role-request");
    this.requestGeneration = generation;
    try {
      const reply = await wsRequest<CustomRolesState>("get_custom_roles", {});
      if (this.mounted && generation === this.requestGeneration && reply.payload) {
        this.setState({ roles: reply.payload });
      }
    } catch (failure) {
      if (this.mounted && generation === this.requestGeneration) this.setState({ error: failureMessage(failure) });
    }
  }

  /** Click handler of the Refresh roles button. */
  @bind
  protected onRefreshClick(): void {
    void this.refresh();
  }

  /**
   * Loads a role (or a blank draft) into the form.
   *
   * @param nextName - the role to edit, or null for a new one
   * @param nextScope - the scope being edited
   */
  protected loadDraft(nextName: string | null, nextScope: CustomRoleScope): void {
    const { roles } = this.state;
    const scopedDefinition = nextName ? customRoleAt(roles?.[nextScope], nextName) : undefined;
    const definition = scopedDefinition ?? (nextName ? customRoleAt(roles?.effective, nextName) : undefined);
    this.setState({
      editingName: nextName,
      name: nextName ?? "",
      provider: definition?.provider ?? "",
      model: definition?.model ?? "",
      effort: definition?.reasoning_effort ?? "",
      think: definition?.think ?? false,
      toolsMode: definition ? roleToolsMode(definition) : "inherit",
      patterns: JSON.stringify(definition?.tools ?? [], null, 2),
      toolsChanged: false,
      hadScopedDefinition: !!nextName && scopedDefinition !== undefined,
      error: "",
      notice: "",
    });
  }

  /**
   * Shows a failed request; an ambiguous one also reloads the roles.
   *
   * @param failure - the thrown value
   */
  protected reportFailure(failure: unknown): void {
    if (!this.mounted) return;
    const message = failureMessage(failure);
    if (failure instanceof WSRequestError && failure.ambiguous) {
      this.setState({ error: `${message}. The role may have changed; refresh before retrying.` });
      void this.refresh();
    } else this.setState({ error: message });
  }

  /**
   * Validates the draft and saves it as a role.
   *
   * @param event - the form submit event
   */
  @bind
  protected async save(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    const { roles, busy, scope, name, provider, model, effort, think } = this.state;
    const { toolsMode, patterns, toolsChanged, hadScopedDefinition, editingName } = this.state;
    if (busy || !roles) return;
    this.setState({ error: "", notice: "" });
    try {
      if (!name || name !== name.trim()) throw new Error("Enter a role name without leading or trailing spaces.");
      if (!editingName && customRoleAt(roles[scope], name)) {
        throw new Error("This role already exists at this scope. Choose Edit instead.");
      }
      if (!provider || !model) throw new Error("Provider and model are required.");
      const payload: SetCustomRoleRequest = {
        scope,
        name,
        provider,
        model,
        reasoning_effort: effort,
        ...(think ? { think: true } : {}),
      };
      if (toolsChanged || !hadScopedDefinition) Object.assign(payload, roleToolsPayload(toolsMode, patterns));
      this.setState({ busy: true });
      await wsRequest("set_custom_role", payload);
      if (!this.mounted) return;
      this.setState({ editingName: name, hadScopedDefinition: true, toolsChanged: false, notice: "Role saved." });
      void this.refresh();
    } catch (failure) {
      this.reportFailure(failure);
    } finally {
      if (this.mounted) this.setState({ busy: false });
    }
  }

  /** Removes the scoped definition being edited. */
  @bind
  protected async remove(): Promise<void> {
    const { busy, editingName, scope } = this.state;
    if (busy || !editingName) return;
    this.setState({ error: "", notice: "", busy: true });
    try {
      await wsRequest("remove_custom_role", { scope, name: editingName });
      if (!this.mounted) return;
      this.loadDraft(null, scope);
      this.setState({ notice: "Definition removed. A lower-scope definition may now apply." });
      void this.refresh();
    } catch (failure) {
      this.reportFailure(failure);
    } finally {
      if (this.mounted) this.setState({ busy: false });
    }
  }

  /**
   * Submit handler of the form.
   *
   * @param event - the form submit event
   */
  @bind
  protected onSubmit(event: FormEvent<HTMLFormElement>): void {
    void this.save(event);
  }

  /** Click handler of the Remove button. */
  @bind
  protected onRemoveClick(): void {
    void this.remove();
  }

  /**
   * Switches the edited scope, reloading the draft from it.
   *
   * @param event - the select change event
   */
  @bind
  protected onScopeChange(event: ChangeEvent<HTMLSelectElement>): void {
    const next = event.target.value as CustomRoleScope;
    this.setState({ scope: next });
    this.loadDraft(this.state.editingName, next);
  }

  /**
   * Opens an existing role in the form.
   *
   * @param roleName - the role to edit
   */
  @bind
  protected onEditRole(roleName: string): void {
    this.loadDraft(roleName, this.state.scope);
  }

  /** Starts a blank draft. */
  @bind
  protected onNewRole(): void {
    this.loadDraft(null, this.state.scope);
  }

  /**
   * Stores the typed role name.
   *
   * @param event - the input change event
   */
  @bind
  protected onNameChange(event: ChangeEvent<HTMLInputElement>): void {
    this.setState({ name: event.target.value });
  }

  /**
   * Stores the typed provider and resets the dependent effort/think settings.
   *
   * @param event - the input change event
   */
  @bind
  protected onProviderChange(event: ChangeEvent<HTMLInputElement>): void {
    this.setState({ provider: event.target.value, effort: "", think: false });
  }

  /**
   * Stores the typed model and resets the dependent effort/think settings.
   *
   * @param event - the input change event
   */
  @bind
  protected onModelChange(event: ChangeEvent<HTMLInputElement>): void {
    this.setState({ model: event.target.value, effort: "", think: false });
  }

  /**
   * Stores the chosen or typed reasoning effort.
   *
   * @param event - the select or input change event
   */
  @bind
  protected onEffortChange(event: ChangeEvent<HTMLSelectElement | HTMLInputElement>): void {
    this.setState({ effort: event.target.value });
  }

  /**
   * Stores the chosen tools mode.
   *
   * @param event - the select change event
   */
  @bind
  protected onToolsModeChange(event: ChangeEvent<HTMLSelectElement>): void {
    this.setState({ toolsMode: event.target.value as RoleToolsMode, toolsChanged: true });
  }

  /**
   * Stores the typed tool patterns.
   *
   * @param event - the textarea change event
   */
  @bind
  protected onPatternsChange(event: ChangeEvent<HTMLTextAreaElement>): void {
    this.setState({ patterns: event.target.value, toolsChanged: true });
  }

  /**
   * Renders one role of the list.
   *
   * @param roleName - the role
   */
  @bind
  protected renderRole(roleName: string) {
    const { roles, busy } = this.state;
    return (
      <RoleListItem
        key={roleName}
        roleName={roleName}
        sources={roleSources(roles, roleName)}
        effective={customRoleAt(roles?.effective, roleName)}
        busy={busy}
        onEdit={this.onEditRole}
      />
    );
  }

  /**
   * Renders the reasoning-effort control that fits the chosen model.
   *
   * @param levels - the model's effort ladder, or null when it has none
   * @param known - whether the model is in the cached catalog
   */
  protected renderEffort(levels: readonly string[] | null, known: boolean) {
    const { effort, busy, roles } = this.state;
    if (levels !== null) {
      return (
        <label className="block text-xs text-text-subtle">Reasoning effort
          <select className={fieldClass} value={effort} disabled={busy || !roles} onChange={this.onEffortChange}>
            <option value="">Provider default</option>
            {!!effort && !levels.includes(effort) && (
              <option value={effort}>Saved: {effort} (not in current ladder)</option>
            )}
            {levels.map(renderLevelOption)}
          </select>
        </label>
      );
    }
    if (known) {
      return (
        <p className="text-[11px] text-text-subtle">
          This model does not publish an effort control. Any saved effort is preserved until you change the model.
        </p>
      );
    }
    return (
      <label className="block text-xs text-text-subtle">Reasoning effort (optional; model not in cached catalog)
        <input className={fieldClass} value={effort} disabled={busy || !roles} onChange={this.onEffortChange} />
      </label>
    );
  }

  /** Renders the section: scope picker, role list and the role form. */
  public render() {
    const { models } = this.props;
    const { roles, scope, editingName, name, provider, model, toolsMode, patterns, busy, error, notice } = this.state;
    const selectedModel = findModel(models, provider, model);
    const levels = effortLevelsFor(provider, model, selectedModel);
    const providers = providerIDs(models);
    const names = roleNames(roles);
    const providerModels = modelsOfProvider(models, provider);

    return (
      <section data-test-id="custom-roles-editor" className="space-y-3 border-t border-surface pt-4">
        <div className="flex items-center justify-between gap-2">
          <h3 className="text-sm font-semibold text-text">Custom roles</h3>
          <button
            type="button"
            disabled={busy}
            onClick={this.onRefreshClick}
            className="text-xs text-accent disabled:opacity-40"
          >
            Refresh roles
          </button>
        </div>
        <p className="text-[11px] text-text-subtle">
          Named model profiles with optional tool restrictions.
          Roles never bypass permissions, depth or inherited tool limits.
        </p>
        {error && <p role="alert" className="text-xs text-red-400">{error}</p>}
        {notice && <output className="block text-xs text-text-subtle">{notice}</output>}
        {!roles && !error && <p className="text-xs text-text-subtle">Loading roles…</p>}
        <label className="block text-xs text-text-subtle">
          Role scope
          <select
            aria-label="Role scope"
            className={fieldClass}
            value={scope}
            disabled={busy || !roles}
            onChange={this.onScopeChange}
          >
            <option value="global">System (global)</option>
            <option value="workspace" disabled={!roles?.hasWorkspace}>Folder (workspace)</option>
          </select>
        </label>
        <ul className="space-y-1">
          {names.map(this.renderRole)}
        </ul>
        {roles && names.length === 0 && <p className="text-xs text-text-subtle">No custom roles configured.</p>}
        <button
          type="button"
          disabled={busy || !roles}
          onClick={this.onNewRole}
          className="text-xs text-accent disabled:opacity-40"
        >
          New role
        </button>
        <form onSubmit={this.onSubmit} className="space-y-2">
          <label className="block text-xs text-text-subtle">Role name
            <input
              className={fieldClass}
              value={name}
              disabled={busy || !!editingName || !roles}
              onChange={this.onNameChange}
            />
          </label>
          <label className="block text-xs text-text-subtle">Provider
            <input
              className={fieldClass}
              value={provider}
              list={`${this.id}-providers`}
              disabled={busy || !roles}
              onChange={this.onProviderChange}
            />
          </label>
          <datalist id={`${this.id}-providers`}>{providers.map(renderProviderOption)}</datalist>
          <label className="block text-xs text-text-subtle">Model
            <input
              className={fieldClass}
              value={model}
              list={`${this.id}-models`}
              disabled={busy || !roles}
              onChange={this.onModelChange}
            />
          </label>
          <datalist id={`${this.id}-models`}>{providerModels.map(renderModelOption)}</datalist>
          {this.renderEffort(levels, !!selectedModel)}
          <label className="block text-xs text-text-subtle">Allowed tools
            <select
              className={fieldClass}
              value={toolsMode}
              disabled={busy || !roles}
              onChange={this.onToolsModeChange}
            >
              <option value="inherit">No additional role restriction (field omitted)</option>
              <option value="none">No tools ([])</option>
              <option value="patterns">Allowed names and patterns</option>
            </select>
          </label>
          {toolsMode === "patterns" && (
            <label className="block text-xs text-text-subtle">Tool patterns (JSON array)
              <textarea
                className={fieldClass}
                rows={3}
                value={patterns}
                disabled={busy || !roles}
                onChange={this.onPatternsChange}
                placeholder={'["view", "grep", "mcp_*"]'}
              />
            </label>
          )}
          <p className="text-[11px] text-text-subtle">
            Only * and ? are wildcards; matching is case-sensitive. An empty array means no tools.
            Removing a scoped definition may reveal an inherited one.
          </p>
          <div className="flex items-center gap-3">
            <button
              type="submit"
              disabled={busy || !roles || !name || !provider || !model}
              className="text-xs text-accent disabled:opacity-40"
            >
              {busy ? "Saving…" : "Save role"}
            </button>
            <button
              type="button"
              disabled={busy || !editingName || !customRoleAt(roles?.[scope], editingName ?? "")}
              onClick={this.onRemoveClick}
              className="text-xs text-red-400 disabled:opacity-40"
            >
              Remove scoped definition
            </button>
          </div>
        </form>
      </section>
    );
  }
}
