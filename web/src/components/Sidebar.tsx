import { useState, useRef, useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $sessions, $activeSessionID, $busySessions, $config, setActiveSession, removeSession } from "../store";
import { ws } from "../ws";
import { MessageSquare, Plus, Pencil, X, Check, Folder, Trash2 } from "lucide-react";
import { ConfirmDialog } from "./ConfirmDialog";

// Wire shape of a delete_other_sessions reply payload (task #684). Mirrors
// internal/server/protocol.go's DeleteOtherSessionsResult: only DeletedIDs
// were actually removed server-side; FailedIDs stayed put after a
// per-session delete failure inside the handler's loop.
interface DeleteOtherSessionsResult {
  deletedIDs: string[];
  failedIDs: string[];
}

function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}

export function Sidebar() {
  const allSessions = useStore($sessions);
  const sessions = allSessions.filter((s) => !s.ParentSessionID);
  const activeID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const config = useStore($config);
  const [editingID, setEditingID] = useState<string | null>(null);
  const [editTitle, setEditTitle] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const [pendingDelete, setPendingDelete] = useState<{ id: string; title: string } | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [confirmDeleteOthers, setConfirmDeleteOthers] = useState(false);
  const [deleteOthersError, setDeleteOthersError] = useState<string | null>(null);
  const [deletingOthers, setDeletingOthers] = useState(false);

  useEffect(() => {
    if (editingID && inputRef.current) {
      inputRef.current.focus();
      inputRef.current.select();
    }
  }, [editingID]);

  // Pending delete reply handlers, detached on unmount so a reply landing
  // after this component unmounted cannot resolve a dead delete (mirrors
  // SystemPromptModal.save()'s unsubRef pattern in ChatToolbar.tsx, task #683).
  const deleteUnsubRef = useRef<(() => void) | null>(null);
  const deleteOthersUnsubRef = useRef<(() => void) | null>(null);
  useEffect(() => {
    return () => {
      deleteUnsubRef.current?.();
      deleteOthersUnsubRef.current?.();
    };
  }, []);

  function selectSession(id: string) {
    if (editingID === id) return;
    setActiveSession(id);
    ws.send("load_messages", { sessionID: id });
  }

  function newSession() {
    ws.send("create_session");
  }

  function deleteSession(e: React.MouseEvent, id: string, title: string) {
    e.stopPropagation();
    setDeleteError(null);
    setPendingDelete({ id, title: title || "Untitled session" });
  }

  function confirmDelete() {
    if (!pendingDelete) return;
    const target = pendingDelete;
    setDeleteError(null);
    setDeleting(true);
    const msgID = crypto.randomUUID();
    deleteUnsubRef.current?.();
    const unsub = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      unsub();
      deleteUnsubRef.current = null;
      setDeleting(false);
      if (msg.error) {
        setDeleteError(msg.error as string);
        return;
      }
      removeSession(target.id);
      if (activeID === target.id) setActiveSession(null);
      setPendingDelete(null);
    });
    deleteUnsubRef.current = unsub;
    ws.send("delete_session", { sessionID: target.id }, msgID);
  }

  function confirmDeleteOtherSessions() {
    if (!activeID) return;
    setDeleteOthersError(null);
    setDeletingOthers(true);
    const msgID = crypto.randomUUID();
    deleteOthersUnsubRef.current?.();
    const unsub = ws.on("*", (msg) => {
      if (msg.id !== msgID) return;
      unsub();
      deleteOthersUnsubRef.current = null;
      setDeletingOthers(false);
      if (msg.error) {
        setDeleteOthersError(msg.error as string);
        return;
      }
      // Only drop rows the server actually confirms deleted (task #684) —
      // a partial failure (deletedIDs missing some non-kept session) must
      // leave the survivor's row in place, not vanish on a blanket "ok".
      const result = msg.payload as DeleteOtherSessionsResult | undefined;
      const deletedIDs = new Set(result?.deletedIDs ?? []);
      for (const s of allSessions) {
        if (s.ID !== activeID && deletedIDs.has(s.ID)) removeSession(s.ID);
      }
      if (result && result.failedIDs.length > 0) {
        setDeleteOthersError(
          `${result.failedIDs.length} session${result.failedIDs.length === 1 ? "" : "s"} could not be deleted`
        );
        return;
      }
      setConfirmDeleteOthers(false);
    });
    deleteOthersUnsubRef.current = unsub;
    ws.send("delete_other_sessions", { keepID: activeID }, msgID);
  }

  function startEditing(e: React.MouseEvent, id: string, title: string) {
    e.stopPropagation();
    setEditingID(id);
    setEditTitle(title || "Untitled session");
  }

  function saveRename() {
    if (editingID) {
      ws.send("rename_session", { sessionID: editingID, title: editTitle.trim() || "Untitled session" });
      setEditingID(null);
    }
  }

  function handleKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Enter") {
      saveRename();
    } else if (e.key === "Escape") {
      setEditingID(null);
    }
  }

  return (
    <aside className="w-64 bg-base-subtle border-r border-surface flex flex-col overflow-hidden shrink-0" data-test-id="sidebar">
      {/* Header */}
      <div className="flex items-center justify-between px-6 py-5 border-b border-surface" data-test-id="sidebar-header">
        <div className="flex flex-col gap-0.5">
          <span className="text-xl font-black text-accent tracking-tighter">Crush~</span>
          <span className="text-[10px] text-text-subtle font-mono opacity-60">#{__GIT_COUNT__} · {__GIT_COMMIT__}</span>
          <span className="text-[10px] text-text-subtle font-mono opacity-50">{__GIT_BRANCH__}</span>
        </div>
        <div className="flex items-center gap-1.5">
          <button
            onClick={newSession}
            title="New session"
            data-test-id="sidebar-new-session"
            className="flex items-center gap-2 px-4 py-2 bg-accent-fill text-white/90 text-sm font-bold rounded-xl hover:bg-accent/90 active:scale-95 transition-all shadow-sm"
          >
            <Plus size={18} />
            New
          </button>
          <button
            onClick={() => {
              setDeleteOthersError(null);
              setConfirmDeleteOthers(true);
            }}
            disabled={!activeID || sessions.length <= 1}
            title="Delete all sessions except the current one"
            data-test-id="sidebar-delete-others"
            className="flex items-center justify-center w-9 h-9 rounded-xl text-text-subtle hover:text-red hover:bg-red/10 disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-text-subtle transition-all"
          >
            <Trash2 size={16} />
          </button>
        </div>
      </div>

      {/* Working directory */}
      {config?.cwd && (
        <div className="flex items-center gap-1.5 px-6 py-2 border-t border-surface/50 text-[11px] text-text-subtle font-mono truncate" title={config.cwd}>
          <Folder size={11} className="shrink-0" />
          <span className="truncate">{config.cwd}</span>
        </div>
      )}

      {/* Session list */}
      <div className="flex-1 overflow-y-auto py-3 px-3" data-test-id="sidebar-session-list">
        {sessions.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-16 px-6 text-center" data-test-id="sidebar-empty">
            <div className="w-12 h-12 rounded-2xl bg-base-overlay flex items-center justify-center mb-4 text-text-subtle">
              <MessageSquare size={24} />
            </div>
            <p className="text-text-muted text-base font-semibold">No sessions yet</p>
            <p className="text-text-subtle text-sm mt-1.5">Click New to get started</p>
          </div>
        ) : (
          sessions.map((s) => {
            const isBusy = busySessions.has(s.ID);
            const totalTokens = s.PromptTokens + s.CompletionTokens;
            const isActive = s.ID === activeID;
            const isEditing = editingID === s.ID;

            return (
              <div
                key={s.ID}
                onClick={() => selectSession(s.ID)}
                onDoubleClick={(e) => startEditing(e, s.ID, s.Title)}
                data-test-id={`session-${s.ID}`}
                data-session-id={s.ID}
                className={`group relative px-4 py-4 rounded-xl cursor-pointer transition-all mb-1 ${
                  isActive
                    ? "bg-canvas shadow-sm border border-accent/20"
                    : "hover:bg-canvas/50 border border-transparent"
                }`}
              >
                <div className={`flex items-start gap-3 ${!isEditing ? "pr-12" : ""}`}>
                  {isBusy && !isEditing && (
                    <span
                      data-test-id={`session-busy-${s.ID}`}
                      className="w-2 h-2 rounded-full bg-accent shrink-0 animate-pulse mt-2"
                    />
                  )}
                  {isEditing ? (
                    <div className="flex-1 flex flex-col gap-1.5 min-w-0" onClick={(e) => e.stopPropagation()}>
                      <input
                        ref={inputRef}
                        value={editTitle}
                        onChange={(e) => setEditTitle(e.target.value)}
                        onKeyDown={handleKeyDown}
                        data-test-id="session-edit-input"
                        className="font-medium w-full bg-canvas border border-accent rounded-lg px-2 py-1 outline-none shadow-sm"
                        style={{ fontSize: "var(--chat-font-size)" }}
                      />
                      <div className="flex gap-1.5">
                        <button
                          onClick={(e) => { e.stopPropagation(); saveRename(); }}
                          title="Save (Enter)"
                          data-test-id="session-edit-save"
                          className="flex items-center gap-1 px-2 py-1 rounded-lg btn-primary text-xs font-semibold"
                        >
                          <Check size={12} /> Save
                        </button>
                        <button
                          onClick={(e) => { e.stopPropagation(); setEditingID(null); }}
                          title="Cancel (Esc)"
                          data-test-id="session-edit-cancel"
                          className="flex items-center gap-1 px-2 py-1 rounded-lg bg-base-overlay text-text-subtle hover:text-text border border-surface text-xs"
                        >
                          <X size={12} /> Cancel
                        </button>
                      </div>
                    </div>
                  ) : (
                    <div
                      data-test-id={`session-title-${s.ID}`}
                      className={`font-semibold ${isActive ? "text-accent" : "text-text"}`}
                      style={{ fontSize: "var(--chat-font-size)" }}
                    >
                      {s.Title || "Untitled session"}
                    </div>
                  )}
                </div>
                {!isEditing && (
                  <div className="flex items-center gap-2.5 mt-1 text-text-subtle pl-0 font-medium" style={{ fontSize: "var(--chat-font-size)" }}>
                    <span data-test-id={`session-msg-count-${s.ID}`}>{s.MessageCount} msg{s.MessageCount !== 1 ? "s" : ""}</span>
                    {totalTokens > 0 && (
                      <>
                        <span>·</span>
                        <span data-test-id={`session-tokens-${s.ID}`}>{formatTokens(totalTokens)} tok</span>
                      </>
                    )}
                  </div>
                )}

                {!isEditing && (
                  <div className="session-hover-btns">
                    <button
                      onClick={(e) => startEditing(e, s.ID, s.Title)}
                      title="Rename session"
                      data-test-id={`session-rename-${s.ID}`}
                      className="sidebar-icon-btn"
                    >
                      <Pencil size={14} />
                    </button>
                    <button
                      onClick={(e) => deleteSession(e, s.ID, s.Title)}
                      title="Delete session"
                      data-test-id={`session-delete-${s.ID}`}
                      className="sidebar-icon-btn-danger"
                    >
                      <X size={16} />
                    </button>
                  </div>
                )}
              </div>
            );
          })
        )}
      </div>

      {pendingDelete && (
        <ConfirmDialog
          title="Delete session"
          message={`"${pendingDelete.title}" and all its messages will be permanently deleted.`}
          confirmLabel={deleting ? "Deleting…" : "Delete"}
          onConfirm={confirmDelete}
          onCancel={() => { setPendingDelete(null); setDeleteError(null); }}
          error={deleteError}
          busy={deleting}
        />
      )}

      {confirmDeleteOthers && (
        <ConfirmDialog
          title="Delete all other sessions"
          message="Delete all sessions except the current one? This cannot be undone."
          confirmLabel={deletingOthers ? "Deleting…" : "Delete all"}
          onConfirm={confirmDeleteOtherSessions}
          onCancel={() => { setConfirmDeleteOthers(false); setDeleteOthersError(null); }}
          error={deleteOthersError}
          busy={deletingOthers}
        />
      )}
    </aside>
  );
}
