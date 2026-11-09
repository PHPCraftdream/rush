import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { FormEvent } from "react";
import type { CustomRole, CustomRoleScope, CustomRolesState, SetCustomRoleRequest } from "../types";
import { roleToolsMode, roleToolsPayload, type RoleToolsMode } from "../customRoleTypes";
import { effortLevelsFor } from "../effort";
import { ws, wsRequest, WSRequestError } from "../ws";
import type { ModelItem } from "./ModelSelector";

const fieldClass = "w-full rounded-lg border border-surface bg-canvas px-2.5 py-1.5 text-xs text-text disabled:opacity-40";

export function CustomRolesEditor({ models }: { models: ModelItem[] }) {
  const id = useId();
  const mounted = useRef(false);
  const requestGeneration = useRef<symbol | null>(null);
  const [state, setState] = useState<CustomRolesState | null>(null);
  const [scope, setScope] = useState<CustomRoleScope>("global");
  const [editingName, setEditingName] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [provider, setProvider] = useState("");
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  const [think, setThink] = useState(false);
  const [toolsMode, setToolsMode] = useState<RoleToolsMode>("inherit");
  const [patterns, setPatterns] = useState("[]");
  const [toolsChanged, setToolsChanged] = useState(false);
  const [hadScopedDefinition, setHadScopedDefinition] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const refresh = useCallback(async () => {
    const generation = Symbol("role-request");
    requestGeneration.current = generation;
    try {
      const reply = await wsRequest<CustomRolesState>("get_custom_roles", {});
      if (mounted.current && generation === requestGeneration.current && reply.payload) setState(reply.payload);
    } catch (failure) {
      if (mounted.current && generation === requestGeneration.current) {
        setError(failure instanceof Error ? failure.message : String(failure));
      }
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    const unsub = ws.on("custom_roles", (message) => {
      if (message.id || !message.payload) return;
      requestGeneration.current = null;
      setState(message.payload as CustomRolesState);
    });
    const reconnect = ws.on("_connected", () => { void refresh(); });
    queueMicrotask(() => { if (mounted.current) void refresh(); });
    return () => {
      mounted.current = false;
      requestGeneration.current = null;
      unsub();
      reconnect();
    };
  }, [refresh]);

  const selectedModel = useMemo(() => models.find((item) => item.providerID === provider && item.modelID === model), [models, provider, model]);
  const levels = effortLevelsFor(provider, model, selectedModel);
  const providers = useMemo(() => [...new Set(models.map((item) => item.providerID))].sort(), [models]);
  const names = useMemo(() => [...new Set([
    ...Object.keys(state?.global ?? {}), ...Object.keys(state?.workspace ?? {}), ...Object.keys(state?.effective ?? {}),
  ])].sort(), [state]);

  function loadDraft(nextName: string | null, nextScope: CustomRoleScope) {
    const definition: CustomRole | undefined = nextName ? (state?.[nextScope][nextName] ?? state?.effective[nextName]) : undefined;
    setEditingName(nextName);
    setName(nextName ?? "");
    setProvider(definition?.provider ?? "");
    setModel(definition?.model ?? "");
    setEffort(definition?.reasoning_effort ?? "");
    setThink(definition?.think ?? false);
    setToolsMode(definition ? roleToolsMode(definition) : "inherit");
    setPatterns(JSON.stringify(definition?.tools ?? [], null, 2));
    setToolsChanged(false);
    setHadScopedDefinition(!!nextName && state?.[nextScope][nextName] !== undefined);
    setError("");
    setNotice("");
  }

  function reportFailure(failure: unknown) {
    if (!mounted.current) return;
    const message = failure instanceof Error ? failure.message : String(failure);
    if (failure instanceof WSRequestError && failure.ambiguous) {
      setError(`${message}. The role may have changed; refresh before retrying.`);
      void refresh();
    } else setError(message);
  }

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy || !state) return;
    setError("");
    setNotice("");
    try {
      if (!name || name !== name.trim()) throw new Error("Enter a role name without leading or trailing spaces.");
      if (!editingName && state[scope][name]) throw new Error("This role already exists at this scope. Choose Edit instead.");
      if (!provider || !model) throw new Error("Provider and model are required.");
      const payload: SetCustomRoleRequest = {
        scope, name, provider, model,
        reasoning_effort: effort,
        ...(think ? { think: true } : {}),
      };
      if (toolsChanged || !hadScopedDefinition) Object.assign(payload, roleToolsPayload(toolsMode, patterns));
      setBusy(true);
      await wsRequest("set_custom_role", payload);
      if (!mounted.current) return;
      setEditingName(name);
      setHadScopedDefinition(true);
      setToolsChanged(false);
      setNotice("Role saved.");
      void refresh();
    } catch (failure) {
      reportFailure(failure);
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  async function remove() {
    if (busy || !editingName) return;
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await wsRequest("remove_custom_role", { scope, name: editingName });
      if (!mounted.current) return;
      loadDraft(null, scope);
      setNotice("Definition removed. A lower-scope definition may now apply.");
      void refresh();
    } catch (failure) {
      reportFailure(failure);
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  return (
    <section data-test-id="custom-roles-editor" className="space-y-3 border-t border-surface pt-4">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-sm font-semibold text-text">Custom roles</h3>
        <button type="button" disabled={busy} onClick={() => { void refresh(); }} className="text-xs text-accent disabled:opacity-40">Refresh roles</button>
      </div>
      <p className="text-[11px] text-text-subtle">Named model profiles with optional tool restrictions. Roles never bypass permissions, depth or inherited tool limits.</p>
      {error && <p role="alert" className="text-xs text-red-400">{error}</p>}
      {notice && <output className="block text-xs text-text-subtle">{notice}</output>}
      {!state && !error && <p className="text-xs text-text-subtle">Loading roles…</p>}
      <label className="block text-xs text-text-subtle">
        Role scope
        <select aria-label="Role scope" className={fieldClass} value={scope} disabled={busy || !state} onChange={(event) => {
          const next = event.target.value as CustomRoleScope;
          setScope(next);
          loadDraft(editingName, next);
        }}>
          <option value="global">System (global)</option>
          <option value="workspace" disabled={!state?.hasWorkspace}>Folder (workspace)</option>
        </select>
      </label>
      <ul className="space-y-1">
        {names.map((roleName) => {
          const effective = state?.effective[roleName];
          const sources = [state?.global[roleName] && "global", state?.workspace[roleName] && "workspace"].filter(Boolean).join(", ") || "inherited";
          return <li key={roleName} className="flex items-center justify-between gap-2 text-xs">
            <span className="min-w-0 break-all text-text">{roleName} <span className="text-text-subtle">({sources}) {effective?.provider}/{effective?.model}</span></span>
            <button type="button" disabled={busy} onClick={() => loadDraft(roleName, scope)} aria-label={`Edit role ${roleName}`} className="text-accent disabled:opacity-40">Edit</button>
          </li>;
        })}
      </ul>
      {state && names.length === 0 && <p className="text-xs text-text-subtle">No custom roles configured.</p>}
      <button type="button" disabled={busy || !state} onClick={() => loadDraft(null, scope)} className="text-xs text-accent disabled:opacity-40">New role</button>
      <form onSubmit={(event) => { void save(event); }} className="space-y-2">
        <label className="block text-xs text-text-subtle">Role name
          <input className={fieldClass} value={name} disabled={busy || !!editingName || !state} onChange={(event) => setName(event.target.value)} />
        </label>
        <label className="block text-xs text-text-subtle">Provider
          <input className={fieldClass} value={provider} list={`${id}-providers`} disabled={busy || !state} onChange={(event) => { setProvider(event.target.value); setEffort(""); setThink(false); }} />
        </label>
        <datalist id={`${id}-providers`}>{providers.map((value) => <option key={value} value={value}>{value}</option>)}</datalist>
        <label className="block text-xs text-text-subtle">Model
          <input className={fieldClass} value={model} list={`${id}-models`} disabled={busy || !state} onChange={(event) => { setModel(event.target.value); setEffort(""); setThink(false); }} />
        </label>
        <datalist id={`${id}-models`}>{models.filter((item) => item.providerID === provider).map((item) => <option key={item.key} value={item.modelID}>{item.name}</option>)}</datalist>
        {levels !== null ? <label className="block text-xs text-text-subtle">Reasoning effort
          <select className={fieldClass} value={effort} disabled={busy || !state} onChange={(event) => setEffort(event.target.value)}>
            <option value="">Provider default</option>
            {!!effort && !levels.includes(effort) && <option value={effort}>Saved: {effort} (not in current ladder)</option>}
            {levels.map((level) => <option key={level} value={level}>{level}</option>)}
          </select>
        </label> : selectedModel ? <p className="text-[11px] text-text-subtle">This model does not publish an effort control. Any saved effort is preserved until you change the model.</p> : <label className="block text-xs text-text-subtle">Reasoning effort (optional; model not in cached catalog)
          <input className={fieldClass} value={effort} disabled={busy || !state} onChange={(event) => setEffort(event.target.value)} />
        </label>}
        <label className="block text-xs text-text-subtle">Allowed tools
          <select className={fieldClass} value={toolsMode} disabled={busy || !state} onChange={(event) => { setToolsMode(event.target.value as RoleToolsMode); setToolsChanged(true); }}>
            <option value="inherit">No additional role restriction (field omitted)</option>
            <option value="none">No tools ([])</option>
            <option value="patterns">Allowed names and patterns</option>
          </select>
        </label>
        {toolsMode === "patterns" && <label className="block text-xs text-text-subtle">Tool patterns (JSON array)
          <textarea className={fieldClass} rows={3} value={patterns} disabled={busy || !state} onChange={(event) => { setPatterns(event.target.value); setToolsChanged(true); }} placeholder={'["view", "grep", "mcp_*"]'} />
        </label>}
        <p className="text-[11px] text-text-subtle">Only * and ? are wildcards; matching is case-sensitive. An empty array means no tools. Removing a scoped definition may reveal an inherited one.</p>
        <div className="flex items-center gap-3">
          <button type="submit" disabled={busy || !state || !name || !provider || !model} className="text-xs text-accent disabled:opacity-40">{busy ? "Saving…" : "Save role"}</button>
          <button type="button" disabled={busy || !editingName || !state?.[scope][editingName]} onClick={() => { void remove(); }} className="text-xs text-red-400 disabled:opacity-40">Remove scoped definition</button>
        </div>
      </form>
    </section>
  );
}
