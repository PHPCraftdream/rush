import { useState, useRef, useEffect, useMemo } from "react";
import { useStore } from "@nanostores/react";
import { BrainCircuit, Zap, Hammer, ShieldCheck, ChevronLeft, ChevronRight, Undo2 } from "lucide-react";
import {
  $config,
  $recentSmartModels,
  $recentFastModels,
  $recentWorkerModels,
  $recentReviewerModels,
  trackModelUsage,
  removeRecentModel,
  getDefaultModelKey,
  setSessionModel,
  setSessionRoleEffort,
  clearSessionModelSlot,
  type ModelRole,
} from "../store";
import type { ConfigPayload, Session } from "../types";
import { effortLevelsFor, defaultEffortFor, supportsEffort, clampEffort } from "../effort";

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
const ROLE_RECENT_STORE: Record<ModelRole, typeof $recentSmartModels> = {
  smart: $recentSmartModels,
  fast: $recentFastModels,
  worker: $recentWorkerModels,
  reviewer: $recentReviewerModels,
};

// Effort levels in cycle order: left arrow decrements, right arrow increments
// Effort tiers and the model-capability rules now live in ../effort so the
// Default-models modal cannot drift from this selector. Labels mirror our
// short-code convention: oh / ox / oxx -> high / xhigh / max.
const EFFORT_LABELS: Record<string, string> = {
  low: "L",
  medium: "M",
  high: "H",
  xhigh: "X",
  max: "XX",
};


// ── Types ─────────────────────────────────────────────────────────────────────

export interface ModelItem {
  key: string;
  providerID: string;
  providerName: string;
  providerType: string;
  modelID: string;
  name: string;
  contextWindow: number;
  enabled: boolean; // provider has an API key configured
}

export interface ProviderGroup {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  models: ModelItem[];
}

// ── Helper functions ──────────────────────────────────────────────────────────

// Builds a list of provider groups, each with their models
export function buildProviderGroups(config: ConfigPayload | null): ProviderGroup[] {
  const groups: ProviderGroup[] = [];
  const seen = new Set<string>();
  for (const [providerID, p] of Object.entries(config?.providers ?? {})) {
    const enabled = p.enabled ?? false;
    const providerName = p.name || providerID;
    const providerType = p.type ?? "";
    const models: ModelItem[] = [];
    for (const m of (p.models ?? [])) {
      const key = `${providerID}:::${m.id}`;
      if (!seen.has(key)) {
        seen.add(key);
        models.push({ key, providerID, providerName, providerType, modelID: m.id, name: m.name || m.id, contextWindow: m.contextWindow ?? 0, enabled });
      }
    }
    // Providers without an API key can't run a model yet — keep them out of
    // model selection entirely (CLI providers don't need a key, so they're
    // exempt). Configuring a key is done in the Providers settings modal.
    if (models.length > 0 && (enabled || providerType === "cli")) {
      groups.push({ id: providerID, name: providerName, type: providerType, enabled, models });
    }
  }
  groups.sort((a, b) => a.name.localeCompare(b.name));
  return groups;
}

// Builds a flat list of all models from all providers
export function buildModelList(config: ConfigPayload | null): ModelItem[] {
  return buildProviderGroups(config).flatMap(g => g.models);
}

// ── ModelRow ──────────────────────────────────────────────────────────────────

function ModelRow({ model, isSelected, onSelect }: { model: ModelItem, isSelected: boolean, onSelect: (m: ModelItem) => void }) {
  const disabled = !model.enabled;
  return (
    <button
      onClick={() => onSelect(model)}
      data-test-id={`model-item-${model.key}`}
      className={`w-full text-left px-3 py-2 transition-colors border-b border-surface/30 last:border-0 ${
        disabled ? "opacity-50 hover:bg-base-overlay" : isSelected ? "bg-accent/5 hover:bg-accent/8" : "hover:bg-base-overlay"
      }`}
    >
      <div className={`text-sm font-medium truncate ${isSelected ? "text-accent" : disabled ? "text-text-subtle" : "text-text"}`}>
        {model.name}
      </div>
    </button>
  );
}

// ── ModelSelector ─────────────────────────────────────────────────────────────

export function ModelSelector({ session, modelType }: { session: Session | null; modelType: ModelRole }) {
  const config = useStore($config);
  const recentKeys = useStore(ROLE_RECENT_STORE[modelType]);

  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  // When non-null, show API key form for this provider
  const ref = useRef<HTMLDivElement>(null);
  const btnRef = useRef<HTMLButtonElement>(null);
  const [dropdownPos, setDropdownPos] = useState<{ left: number; bottom: number }>({ left: 0, bottom: 0 });

  function updatePos() {
    if (!btnRef.current) return;
    const r = btnRef.current.getBoundingClientRect();
    const width = 520;
    const margin = 8;
    const left = Math.min(r.left, window.innerWidth - width - margin);
    setDropdownPos({ left: Math.max(margin, left), bottom: window.innerHeight - r.top + 8 });
  }

  const allModels = useMemo(() => buildModelList(config), [config]);
  const providerGroups = useMemo(() => buildProviderGroups(config), [config]);
  const defaultKey = useMemo(() => getDefaultModelKey(modelType, config), [modelType, config]);

  // Session field names for this role, e.g. "worker" -> WorkerModelProvider/
  // WorkerModelID/WorkerModelReasoningEffort — same computed-key convention
  // ScopedModelsModal.tsx's SessionSlotRow already uses, so the two surfaces
  // can't drift on how a role name maps to its Session columns.
  const rolePrefix = `${modelType[0].toUpperCase()}${modelType.slice(1)}`;
  const providerField = `${rolePrefix}ModelProvider` as keyof Session;
  const idField = `${rolePrefix}ModelID` as keyof Session;
  const effortField = `${rolePrefix}ModelReasoningEffort` as keyof Session;

  // Get current key from session record if available, else use global default
  const sessionProvider = session?.[providerField] as string | undefined;
  const sessionModelID = session?.[idField] as string | undefined;
  const hasSessionOverride = !!(sessionProvider && sessionModelID);
  let currentKey = defaultKey;
  if (hasSessionOverride) {
    currentKey = `${sessionProvider}:::${sessionModelID}`;
  }

  const currentEntry = allModels.find(m => m.key === currentKey);
  const displayName = currentEntry?.name ?? currentKey.split(":::")[1] ?? "No model";

  // Get current reasoning effort — model-dependent default (see
  // defaultEffortFor below): "medium" for Claude CLI, "high" for every
  // Z.AI GLM-5.x including the 5.3 tier.
  const currentProvider = currentEntry?.providerID ?? "";
  const currentModelID = currentEntry?.modelID ?? "";
  // Delegates to ../effort so this selector can't drift from the
  // Default-models modal's model-capability rules (see that module's header
  // comment on why a second hand-written copy of these rules is a bug
  // magnet — GLM-5.3/5.3-Flash's low/high/max vocabulary vs. other GLM-5.x's
  // high/max-only would otherwise need to be kept in sync by hand here too).
  const effortLevels: readonly string[] = effortLevelsFor(currentProvider, currentModelID) ?? [];
  let storedEffort = defaultEffortFor(currentProvider, currentModelID);
  if (session) {
    const effort = session[effortField] as string | undefined;
    if (effort) storedEffort = effort;
  }
  const showEffortPicker = supportsEffort(currentProvider, currentModelID);
  // Clamp the displayed effort to what THIS model actually supports, via the
  // same clampEffort ScopedModelsModal.tsx uses — NOT effortLevels[0], which
  // used to disagree with clampEffort's "fall back to defaultEffortFor"
  // semantics whenever a level array's first entry differs from the model's
  // default (exposed by GLM-5.3/5.3-Flash: levels[0] is "low", but the
  // default is "high"). Without this, switching Claude→GLM on a session that
  // stored "medium" leaves the badge showing M (which GLM does not
  // understand) until the user clicks an arrow. The useEffect below persists
  // the clamp back to the session so the backend never sees an unsupported
  // value either.
  //
  // Side effect of this unification, intentional: an invalid stored effort
  // on a Claude CLI slot (e.g. a legacy value) now also clamps to
  // defaultEffortFor's "medium" instead of the old effortLevels[0] ("low") —
  // Claude's levels[0] happened to differ from its own default too, just
  // less visibly than GLM-5.3's. Both model classes now behave identically
  // to ScopedModelsModal.tsx, which is the point.
  const clampedEffort = clampEffort(currentProvider, currentModelID, storedEffort);
  const effortValid = clampedEffort === storedEffort;
  const currentEffort = clampedEffort ?? storedEffort;

  useEffect(() => {
    if (!session || !showEffortPicker) return;
    if (effortValid) return;
    setSessionRoleEffort(session.ID, modelType, currentEffort);
  }, [session?.ID, modelType, showEffortPicker, effortValid, currentEffort]);

  function cycleEffort(direction: 1 | -1) {
    if (!session || !showEffortPicker) return;
    const idx = effortLevels.indexOf(currentEffort);
    const safeIdx = idx === -1 ? 0 : idx;
    const newIdx = (safeIdx + direction + effortLevels.length) % effortLevels.length;
    const newEffort = effortLevels[newIdx];
    setSessionRoleEffort(session.ID, modelType, newEffort);
  }

  const recentModels = useMemo(() => {
    return recentKeys
      .map(k => allModels.find(m => m.key === k))
      .filter((m): m is ModelItem => !!m);
  }, [recentKeys, allModels]);

  const q = search.toLowerCase();

  const searchResults = useMemo(() => {
    if (!q) return [];
    return allModels.filter(m =>
      m.name.toLowerCase().includes(q) ||
      m.providerID.toLowerCase().includes(q) ||
      m.providerName.toLowerCase().includes(q) ||
      m.modelID.toLowerCase().includes(q)
    );
  }, [allModels, q]);

  useEffect(() => {
    if (!open) return;
    updatePos();
    window.addEventListener("resize", updatePos);
    window.addEventListener("scroll", updatePos, true);
    function handler(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    document.addEventListener("mousedown", handler);
    return () => {
      document.removeEventListener("mousedown", handler);
      window.removeEventListener("resize", updatePos);
      window.removeEventListener("scroll", updatePos, true);
    };
  }, [open]);

  const Icon = ROLE_ICON[modelType];
  const title = ROLE_TITLE[modelType];

  function onSelect(m: ModelItem) {
    if (!m.enabled) return; // CLI providers can't be selected without being enabled
    if (session) {
      // setSessionModel touches ONLY this role's slot (task #461/#1061) —
      // filling other slots in from their current/default value here, like
      // this used to do for smart/fast, would re-write them on every switch
      // and freeze them against later folder/system default changes.
      setSessionModel(session.ID, modelType, m.key);
      trackModelUsage(modelType, m.key);
      setOpen(false);
    }
  }

  // Clears this session's override, falling back to inheriting the
  // folder/system default (task #467). Only meaningful — and only shown —
  // when the session currently HAS an explicit override for this slot.
  function onInherit() {
    if (!session) return;
    clearSessionModelSlot(session.ID, modelType);
    setOpen(false);
  }

  if (!session || allModels.length === 0) {
    return (
      <span className="flex items-center gap-1.5 text-xs text-text-subtle bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5" title={title}>
        <Icon size={12} />
        {displayName}
      </span>
    );
  }

  const recentKeySet = new Set(recentKeys);

  return (
    <div ref={ref} className="relative">
      <button
        ref={btnRef}
        onClick={() => { setOpen(o => !o); setSearch(""); }}
        className="flex items-center gap-1.5 text-xs text-text bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 hover:border-accent/50 hover:bg-base-subtle transition-colors"
        title={title}
        data-test-id={`model-selector-${modelType}`}
      >
        <Icon size={12} className="shrink-0" />
        <span className="font-medium truncate max-w-[180px]">{displayName}</span>
        {showEffortPicker && (
          <div
            className="flex items-center gap-0.5 shrink-0 ml-1"
            onClick={e => e.stopPropagation()}
            data-test-id={`reasoning-effort-${modelType}`}
          >
            <button
              onClick={() => cycleEffort(-1)}
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
              {EFFORT_LABELS[currentEffort] ?? "?"}
            </span>
            <button
              onClick={() => cycleEffort(1)}
              className="p-0.5 rounded hover:bg-base-subtle text-text-subtle hover:text-text transition-colors"
              title={`Reasoning effort: ${currentEffort} (click to increase)`}
              data-test-id={`reasoning-effort-${modelType}-increase`}
            >
              <ChevronRight size={12} strokeWidth={2.5} />
            </button>
          </div>
        )}
        <span className="text-text-subtle ml-auto">{open ? "▴" : "▾"}</span>
      </button>
      {open && (
        <div
          data-test-id="model-dropdown"
          style={{ position: "fixed", left: dropdownPos.left, bottom: dropdownPos.bottom, width: 520, zIndex: 9999 }}
          className="bg-canvas border border-surface rounded-xl shadow-xl overflow-hidden"
        >
            <div className="max-h-[480px] overflow-y-auto">
              {q ? (
                searchResults.length === 0 ? (
                  <p className="text-text-subtle text-sm text-center py-4">No models found</p>
                ) : (
                  searchResults.map(m => (
                    <ModelRow key={m.key} model={m} isSelected={m.key === currentKey} onSelect={onSelect} />
                  ))
                )
              ) : (
                <>
                  {hasSessionOverride && (
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
                      {recentModels.map(m => (
                        <div key={m.key} className="flex items-center group/row">
                          <div className="flex-1 min-w-0">
                            <ModelRow model={m} isSelected={m.key === currentKey} onSelect={onSelect} />
                          </div>
                          <button
                            onClick={e => { e.stopPropagation(); removeRecentModel(modelType, m.key); }}
                            title="Remove from recent"
                            className="shrink-0 px-2 py-2 text-text-subtle hover:text-red opacity-0 group-hover/row:opacity-100 transition-opacity text-xs"
                          >
                            ✕
                          </button>
                        </div>
                      ))}
                      <div className="h-px bg-surface/40 my-1" />
                    </div>
                  )}
                  {providerGroups.map(group => {
                    const groupModels = group.models.filter(m => !recentKeySet.has(m.key));
                    if (groupModels.length === 0) return null;
                    return (
                      <div key={group.id} className="py-1">
                        <div className="px-3 py-1.5 flex items-center gap-2">
                          <span className="text-[10px] font-bold text-text-muted uppercase tracking-wider">{group.name}</span>
                        </div>
                        {groupModels.map(m => (
                          <ModelRow key={m.key} model={m} isSelected={m.key === currentKey} onSelect={onSelect} />
                        ))}
                      </div>
                    );
                  })}
                </>
              )}
            </div>
            <div className="p-2.5 border-t border-surface/40">
              <input
                autoFocus
                value={search}
                onChange={e => setSearch(e.target.value)}
                placeholder="Search models…"
                className="w-full bg-base-overlay border border-surface rounded-lg px-2.5 py-1.5 text-sm text-text outline-none focus:border-accent transition-colors placeholder:text-text-subtle"
              />
            </div>
        </div>
      )}
    </div>
  );
}
