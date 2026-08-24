import { useState, useEffect, useCallback, useMemo, useRef } from "react";
import { useStore } from "@nanostores/react";
import { Minimize2, X, CheckCheck, ScrollText, Plug, Sun, Moon, Settings, ServerCog, FileText, Headphones, Eye, ChevronsDownUp, SlidersHorizontal, ArrowUpCircle, MoreHorizontal } from "lucide-react";
import { $sitter, stopSitter } from "../sitter";
import {
  $sessions,
  $activeSessionID,
  $busySessions,
  $summarizeQueued,
  $config,
  $updateAvailable,
  summarizeSession,
  cancelQueuedSummarize,
  setTheme,
  setKeepAliveEnabled,
  getDefaultModelKey,
  collapseAllSpoilers,
} from "../store";
import { ModelSelector, buildModelList } from "./ModelSelector";
import { StatusBar } from "./StatusBar";
import { LogsModal } from "./LogsModal";
import { MCPSettings } from "./MCPSettings";
import { SettingsModal } from "./SettingsModal";
import { ProvidersModal } from "./ProvidersModal";
import { ScopedModelsModal } from "./ScopedModelsModal";
import { ws } from "../ws";

// ── System Prompt Modal ───────────────────────────────────────────────────────

function SystemPromptModal({ sessionID, onClose }: { sessionID: string; onClose: () => void }) {
  const [original, setOriginal] = useState<string>("");
  const [draft, setDraft] = useState<string>("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const dirty = draft !== original;

  // Pending save reply handler, detached on unmount so a reply landing
  // after this modal was closed cannot resolve a dead save (mirrors
  // MCPForm.submit()'s unsubRef pattern in MCPSettings.tsx).
  const unsubRef = useRef<(() => void) | null>(null);
  useEffect(() => {
    return () => unsubRef.current?.();
  }, []);

  useEffect(() => {
    const unsub = ws.on("system_prompt", (msg) => {
      const p = msg.payload as { content?: string } | undefined;
      const c = p?.content ?? "";
      setOriginal(c);
      setDraft(c);
      setLoading(false);
      unsub();
    });
    ws.send("get_system_prompt", { sessionID });

    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("keydown", onKey);
    return () => {
      unsub();
      document.removeEventListener("keydown", onKey);
    };
  }, [sessionID, onClose]);

  function save() {
    setError(null);
    setSaving(true);
    const savedDraft = draft;
    const msgID = crypto.randomUUID();
    unsubRef.current?.();
    const unsub = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      unsub();
      unsubRef.current = null;
      setSaving(false);
      if (msg.error) {
        setError(msg.error as string);
      } else {
        setOriginal(savedDraft);
      }
    });
    unsubRef.current = unsub;
    ws.send("set_system_prompt", { sessionID, content: draft }, msgID);
  }

  function reset() {
    setDraft(original);
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        className="bg-canvas border border-surface rounded-2xl shadow-xl flex flex-col w-full max-w-3xl mx-4 max-h-[85vh]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-6 py-4 border-b border-surface shrink-0">
          <h2 className="text-base font-semibold text-text">System Prompt</h2>
          <button
            onClick={onClose}
            className="text-text-subtle hover:text-text transition-colors text-xl leading-none"
          >
            ×
          </button>
        </div>
        <div className="flex-1 overflow-hidden p-4">
          {loading ? (
            <p className="text-text-subtle text-sm p-2">Loading…</p>
          ) : (
            <textarea
              className="w-full h-full min-h-[400px] text-xs font-mono text-text bg-base-overlay border border-surface rounded-xl p-3 resize-none outline-none focus:border-accent/50 leading-relaxed"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              spellCheck={false}
            />
          )}
        </div>
        <div className="flex items-center justify-end gap-3 px-6 py-4 border-t border-surface shrink-0">
          {error && (
            <span className="flex-1 text-sm text-red truncate" title={error}>
              {error}
            </span>
          )}
          {dirty && (
            <button
              onClick={reset}
              className="px-4 py-2 text-sm text-text-subtle hover:text-text transition-colors rounded-xl hover:bg-base-overlay"
            >
              Reset
            </button>
          )}
          <button
            onClick={save}
            disabled={!dirty || saving}
            className="px-4 py-2 text-sm font-medium bg-accent-fill text-white/90 rounded-xl hover:opacity-90 transition-opacity disabled:opacity-40 disabled:cursor-not-allowed"
          >
            Save
          </button>
        </div>
      </div>
    </div>
  );
}

function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}

function pctColor(pct: number): string {
  if (pct >= 85) return "text-red";
  if (pct >= 60) return "text-yellow";
  return "text-green";
}

export function ChatToolbar() {
  const sessions = useStore($sessions);
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const summarizeQueued = useStore($summarizeQueued);
  const config = useStore($config);
  const updateInfo = useStore($updateAvailable);
  const sitter = useStore($sitter);

  // Modal state
  const [showSystemPrompt, setShowSystemPrompt] = useState(false);
  const closeSystemPrompt = useCallback(() => setShowSystemPrompt(false), []);
  const [showMCPSettings, setShowMCPSettings] = useState(false);
  const closeMCPSettings = useCallback(() => setShowMCPSettings(false), []);
  const [showSettings, setShowSettings] = useState(false);
  const closeSettings = useCallback(() => setShowSettings(false), []);
  const [showProviders, setShowProviders] = useState(false);
  const closeProviders = useCallback(() => setShowProviders(false), []);
  const [showScopedModels, setShowScopedModels] = useState(false);
  const closeScopedModels = useCallback(() => setShowScopedModels(false), []);
  const [showLogs, setShowLogs] = useState(false);
  const closeLogs = useCallback(() => setShowLogs(false), []);

  // More dropdown state
  const [moreMenuOpen, setMoreMenuOpen] = useState(false);
  const moreMenuRef = useRef<HTMLDivElement>(null);

  // More dropdown close handlers
  useEffect(() => {
    if (!moreMenuOpen) return;
    function handleOutsideClick(e: MouseEvent) {
      if (moreMenuRef.current && !moreMenuRef.current.contains(e.target as Node)) {
        setMoreMenuOpen(false);
      }
    }
    function handleEscape(e: KeyboardEvent) {
      if (e.key === "Escape") {
        setMoreMenuOpen(false);
      }
    }
    document.addEventListener("mousedown", handleOutsideClick);
    document.addEventListener("keydown", handleEscape);
    return () => {
      document.removeEventListener("mousedown", handleOutsideClick);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [moreMenuOpen]);

  const activeSession = sessions.find((s) => s.ID === activeSessionID) ?? null;
  const isBusy = activeSessionID ? busySessions.has(activeSessionID) : false;
  const isQueued = activeSessionID ? summarizeQueued.has(activeSessionID) : false;
  const hasMessages = (activeSession?.MessageCount ?? 0) > 0;

  const isDark = config?.theme === "dark";
  function toggleTheme() {
    setTheme(isDark ? "light" : "dark");
  }
  // Default ON when the server omits the field (older backend) or sends
  // explicit true; OFF only when the operator has explicitly disabled it.
  const keepAliveOn = config?.keepAliveEnabled !== false;

  const totalTokens = activeSession ? activeSession.PromptTokens + activeSession.CompletionTokens : 0;
  const isSummarized = !!activeSession?.SummaryMessageID;

  const allModels = useMemo(() => buildModelList(config), [config]);
  const effectiveSmartKey = useMemo(() => {
    if (!activeSession) return getDefaultModelKey("smart", config);
    const p = activeSession.SmartModelProvider;
    const m = activeSession.SmartModelID;
    if (p && m) return `${p}:::${m}`;
    return getDefaultModelKey("smart", config);
  }, [activeSession, config]);
  const contextWindow = useMemo(() => {
    if (!effectiveSmartKey) return 0;
    return allModels.find(x => x.key === effectiveSmartKey)?.contextWindow ?? 0;
  }, [effectiveSmartKey, allModels]);

  const activeSmartModelName = useMemo(() => {
    if (!effectiveSmartKey) return null;
    return allModels.find(x => x.key === effectiveSmartKey)?.name ?? null;
  }, [effectiveSmartKey, allModels]);

  const contextPct = contextWindow > 0 ? Math.min(100, Math.round((totalTokens / contextWindow) * 100)) : null;
  // Read-only follow mode: another live crush process holds this session's
  // lock. The toolbar still renders so the operator can see the token
  // counter, status, etc., but every mutation control collapses to a
  // single inline notice so we don't fight the foreign agent.
  const foreignOwned = !!activeSession?.OwnedExternal;

  // No `if (!activeSessionID) return null;` here — deliberately. This
  // toolbar absorbed the old standalone Header.tsx in commit 89a07919,
  // whose message claimed "No behaviour changes other than where each
  // control physically renders", yet it also added exactly that early
  // return, which Header never had. With it, the theme toggle, the
  // settings / MCP / providers / logs / prompt buttons and the model
  // badges were all unreachable until a session was selected. We render
  // with no session again; only genuinely session-bound controls are
  // hidden individually (Compact below; Prompt is conditionally rendered via
  // `{activeSessionID && (` inside the "More" dropdown; the token pill and busy dots are
  // already gated on an active session existing).

  if (foreignOwned) {
    return (
      <div className="px-5 pt-3 pb-1 border-t border-surface bg-canvas shrink-0 flex items-center gap-2 flex-wrap">
        {activeSession && totalTokens > 0 && (
          <span
            data-test-id="header-token-indicator"
            className="text-sm font-medium text-text-subtle bg-base-overlay border border-surface rounded-xl px-3.5 py-2 flex items-center gap-1.5"
            title={`${totalTokens.toLocaleString()} tokens`}
          >
            {formatTokens(totalTokens)}
            {contextPct !== null && (
              <span className={`font-semibold ${pctColor(contextPct)}`}>{contextPct}%</span>
            )}
          </span>
        )}
        {isBusy && (
          <div className="flex items-center gap-2 animate-pulse-dots px-2" title={activeSmartModelName ? `Running ${activeSmartModelName}…` : "Agent is working…"}>
            {activeSmartModelName && (
              <span className="text-xs text-text-subtle font-medium">{activeSmartModelName}</span>
            )}
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
          </div>
        )}
        <div className="flex-1" />
        <StatusBar inline />
      </div>
    );
  }

  return (
    <>
      <div className="px-5 pt-3 pb-1 border-t border-surface bg-canvas shrink-0 flex items-center gap-2 flex-wrap">
        {/* LEFT cluster — tokens + Compact sit together (Compact directly
            operates on the context window the token pill displays) followed
            by the migrated header buttons. */}
        {activeSession && totalTokens > 0 && (
          <span
            data-test-id="header-token-indicator"
            className="text-sm font-medium text-text-subtle bg-base-overlay border border-surface rounded-xl px-3.5 py-2 flex items-center gap-1.5"
            title={`${totalTokens.toLocaleString()} tokens total across all requests in this session (includes system prompt + tool definitions sent each turn)${contextWindow > 0 ? ` · context window: ${contextWindow.toLocaleString()}` : ""}`}
          >
            {formatTokens(totalTokens)}
            {contextPct !== null && (
              <span className={`font-semibold ${pctColor(contextPct)}`}>{contextPct}%</span>
            )}
            {isSummarized && (
              <span data-test-id="header-summarized-badge" title="Session has been summarized"><CheckCheck size={13} className="text-accent" /></span>
            )}
          </span>
        )}

        {/* Compact operates on the active session's history — hidden with
            no session selected. */}
        {activeSessionID && (isQueued ? (
          <button
            onClick={() => cancelQueuedSummarize(activeSessionID)}
            title="Compact is queued — click to cancel"
            className="btn-toolbar text-accent border-accent/30 bg-accent/5 hover:bg-red/10 hover:text-red hover:border-red/30 flex items-center gap-1"
          >
            <Minimize2 size={13} />
            Compact queued
            <X size={11} className="opacity-60" />
          </button>
        ) : (
          <button
            onClick={() => summarizeSession(activeSessionID)}
            disabled={!hasMessages}
            title={isBusy ? "Compact will run after the current task finishes" : "Compact — compress conversation history to free up context window"}
            className="btn-toolbar"
          >
            <Minimize2 size={13} />
            Compact
          </button>
        ))}

        <button
          data-test-id="header-collapse-all-button"
          onClick={collapseAllSpoilers}
          title="Collapse all spoilers — fold every tool group, thinking block, summary and background-job notice in this chat"
          className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          <ChevronsDownUp size={13} />
          <span>Collapse all</span>
        </button>

        {/* More dropdown for rarely-used buttons */}
        <div ref={moreMenuRef} className="relative">
          <button
            data-test-id="header-more-button"
            onClick={() => setMoreMenuOpen(o => !o)}
            title="More — prompt, MCP, providers, default models, settings, logs"
            className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
          >
            <MoreHorizontal size={13} />
            <span>More</span>
          </button>

          {moreMenuOpen && (
            <div className="absolute bottom-full mb-2 left-0 w-52 bg-canvas border border-surface rounded-xl shadow-xl z-50 py-1">
              {activeSessionID && (
                <button
                  data-test-id="header-prompt-button"
                  onClick={() => { setMoreMenuOpen(false); setShowSystemPrompt(true); }}
                  className="w-full text-left px-3 py-2 flex items-center gap-2 text-sm text-text-subtle hover:bg-base-overlay hover:text-text transition-colors"
                  title="View / edit system prompt"
                >
                  <ScrollText size={14} />
                  <span>Prompt</span>
                </button>
              )}
              <button
                data-test-id="header-mcp-button"
                onClick={() => { setMoreMenuOpen(false); setShowMCPSettings(true); }}
                className="w-full text-left px-3 py-2 flex items-center gap-2 text-sm text-text-subtle hover:bg-base-overlay hover:text-text transition-colors"
                title="MCP server settings"
              >
                <Plug size={14} />
                <span>MCP</span>
              </button>
              <button
                data-test-id="header-providers-button"
                onClick={() => { setMoreMenuOpen(false); setShowProviders(true); }}
                className="w-full text-left px-3 py-2 flex items-center gap-2 text-sm text-text-subtle hover:bg-base-overlay hover:text-text transition-colors"
                title="Custom providers"
              >
                <ServerCog size={14} />
                <span>Providers</span>
              </button>
              <button
                data-test-id="header-default-models-button"
                onClick={() => { setMoreMenuOpen(false); setShowScopedModels(true); }}
                className="w-full text-left px-3 py-2 flex items-center gap-2 text-sm text-text-subtle hover:bg-base-overlay hover:text-text transition-colors"
                title="Default models — System / Folder / Session"
              >
                <SlidersHorizontal size={14} />
                <span>Default models</span>
              </button>
              <button
                data-test-id="header-settings-button"
                onClick={() => { setMoreMenuOpen(false); setShowSettings(true); }}
                className="w-full text-left px-3 py-2 flex items-center gap-2 text-sm text-text-subtle hover:bg-base-overlay hover:text-text transition-colors"
                title="Settings"
              >
                <Settings size={14} />
                <span>Settings</span>
              </button>
              <button
                data-test-id="header-logs-button"
                onClick={() => { setMoreMenuOpen(false); setShowLogs(true); }}
                className="w-full text-left px-3 py-2 flex items-center gap-2 text-sm text-text-subtle hover:bg-base-overlay hover:text-text transition-colors"
                title="View logs"
              >
                <FileText size={14} />
                <span>Logs</span>
              </button>
            </div>
          )}
        </div>

        {updateInfo && (
          // Rendered only when the server says a newer release exists — it
          // sends nothing otherwise, so there is no "up to date" state to
          // draw. A link, not a self-updater: the fork is installed by the
          // operator and should not rewrite its own binary underneath a
          // running agent session.
          <a
            data-test-id="header-update-available"
            href="https://github.com/PHPCraftdream/crush/releases"
            target="_blank"
            rel="noreferrer"
            title={`crush ${updateInfo.latest} is available (you have ${updateInfo.current})`}
            className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors bg-base-overlay border-accent/50 text-accent hover:border-accent"
          >
            <ArrowUpCircle size={13} />
            <span>v{updateInfo.latest}</span>
          </a>
        )}

        <button
          data-test-id="header-theme-toggle"
          onClick={toggleTheme}
          title={isDark ? "Switch to light theme" : "Switch to dark theme"}
          className="flex items-center justify-center w-8 h-8 rounded-lg border transition-colors bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        >
          {isDark ? <Sun size={14} /> : <Moon size={14} />}
        </button>

        {/* BT keep-alive: tiny inaudible WebAudio loop that prevents
            Bluetooth headphones from suspending the audio device during
            long agent runs (otherwise they eat the first second of any
            real notification). Backed by Options.KeepAliveEnabled in the
            global crush.json; default ON. */}
        <button
          data-test-id="header-keepalive-toggle"
          onClick={() => setKeepAliveEnabled(!keepAliveOn)}
          title={keepAliveOn
            ? "BT keep-alive ON — inaudible audio loop keeps Bluetooth headphones awake. Click to disable."
            : "BT keep-alive OFF — Bluetooth headphones may suspend during silent periods. Click to enable."}
          className={`flex items-center justify-center w-8 h-8 rounded-lg border transition-colors ${
            keepAliveOn
              ? "bg-yellow/10 border-yellow/40 text-yellow hover:bg-yellow/20"
              : "bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
          }`}
        >
          <Headphones size={14} />
        </button>

        {/* Sitter pill: visible only while the auto-resume loop is armed.
            Shows the session it's watching (initials from the title) and
            the wake interval. Click to disarm. Toggled from the chat input
            via `/sitter [N]`. */}
        {sitter.running && (
          <button
            data-test-id="header-sitter-pill"
            onClick={() => stopSitter()}
            title={`Sitter armed — checking session every ${sitter.intervalMin}m. Resumes the agent if it stalls AND there are open todos. Click to disable. (Or type /sitter in the chat.)`}
            className="flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border bg-accent/10 border-accent/40 text-accent hover:bg-red/10 hover:border-red/40 hover:text-red transition-colors"
          >
            <Eye size={13} />
            <span>Sitter {sitter.intervalMin}m</span>
            <X size={11} className="opacity-60" />
          </button>
        )}

        {isBusy && (
          <div className="flex items-center gap-2 animate-pulse-dots px-2" title={activeSmartModelName ? `Running ${activeSmartModelName}…` : "Agent is working…"}>
            {activeSmartModelName && (
              <span className="text-xs text-text-subtle font-medium">{activeSmartModelName}</span>
            )}
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
            <span className="w-2 h-2 rounded-full bg-accent inline-block" />
          </div>
        )}

        {/* Push the right cluster to the far edge. */}
        <div className="flex-1" />

        {/* RIGHT cluster */}
        <ModelSelector session={activeSession} modelType="smart" />
        <ModelSelector session={activeSession} modelType="fast" />
      </div>

      {/* Modal hosts */}
      {showSystemPrompt && activeSessionID && <SystemPromptModal sessionID={activeSessionID} onClose={closeSystemPrompt} />}
      {showMCPSettings && <MCPSettings onClose={closeMCPSettings} />}
      {showSettings && <SettingsModal onClose={closeSettings} />}
      {showProviders && <ProvidersModal onClose={closeProviders} />}
      {showScopedModels && <ScopedModelsModal onClose={closeScopedModels} activeSession={activeSession} />}
      {showLogs && <LogsModal onClose={closeLogs} />}
    </>
  );
}
