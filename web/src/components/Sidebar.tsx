import type { KeyboardEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { setActiveSession, removeSession } from "../store";
import { ws, wsRequest, sendLoadMessages } from "../ws";
import type { Session } from "../types";
import { configCarburetor } from "../state/config";
import { sessionsCarburetor } from "../state/sessions";
import type { ISessionsState } from "../state/SessionsCarburetor";
import { MessageSquare, Plus, Folder, Trash2 } from "lucide-react";
import { ConfirmDialog } from "./ConfirmDialog";
import { SessionRow } from "./sidebar/SessionRow";

/**
 * Picks the sessions list out of the sessions state.
 *
 * @param state - the sessions state
 */
const selectSessions = (state: TReadonly<ISessionsState>) => state.sessions;

/**
 * Whether a session is a top-level one (not a sub-agent's child).
 *
 * @param s - the session
 */
const isTopLevel = (s: TReadonly<Session>): boolean => !s.ParentSessionID;

// Wire shape of a delete_other_sessions reply payload (task #684). Mirrors
// internal/server/protocol.go's DeleteOtherSessionsResult: only DeletedIDs
// were actually removed server-side; FailedIDs stayed put after a
// per-session delete failure inside the handler's loop.
interface DeleteOtherSessionsResult {
  deletedIDs: string[];
  failedIDs: string[];
}

/** Local state of the sidebar. */
interface ISidebarState {
  editingID: string | null;
  editTitle: string;
  pendingDelete: { id: string; title: string } | null;
  deleteError: string | null;
  deleting: boolean;
  confirmDeleteOthers: boolean;
  deleteOthersError: string | null;
  deletingOthers: boolean;
}

/** Session list with create, rename, delete and delete-others. */
export class Sidebar extends AntiHookComponent<{}, ISidebarState> {
  public state: ISidebarState = {
    editingID: null,
    editTitle: "",
    pendingDelete: null,
    deleteError: null,
    deleting: false,
    confirmDeleteOthers: false,
    deleteOthersError: null,
    deletingOthers: false,
  };

  /** Detached snapshot of the sessions list; unchanged sessions keep their identity. */
  private readonly sessionsView = this.connectSelection(sessionsCarburetor, selectSessions);

  // Continuations of confirmDelete()/confirmDeleteOtherSessions() check this
  // flag after their await so a reply landing after this component
  // unmounted cannot resolve a dead delete (same intent as the disposed
  // continuation guard in ChatToolbar.tsx's SystemPromptModal.save(), task
  // #683; the wsRequest listeners themselves are always detached on settle:
  // reply, disconnect, timeout or a failed send).
  private disposed = false;

  /** Tracks mount state. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("lifecycle", this.markMounted, []);
  }

  /** Clears the disposed flag; returns the cleanup that sets it. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Marks the sidebar as unmounted. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  /**
   * Makes a session the active one and loads its messages.
   *
   * @param id - the session id
   */
  @bind
  protected selectSession(id: string): void {
    if (this.state.editingID === id) return;
    setActiveSession(id);
    sendLoadMessages(id);
  }

  /** Asks the server for a new session. */
  @bind
  protected newSession(): void {
    ws.send("create_session");
  }

  /**
   * Opens the delete confirmation for a session.
   *
   * @param id - the session id
   * @param title - its title
   */
  @bind
  protected deleteSession(id: string, title: string): void {
    this.setState({ deleteError: null, pendingDelete: { id, title: title || "Untitled session" } });
  }

  /** Closes the delete confirmation and forgets its error. */
  @bind
  protected cancelDelete(): void {
    this.setState({ pendingDelete: null, deleteError: null });
  }

  /** Deletes the session pending confirmation. */
  @bind
  protected async confirmDelete(): Promise<void> {
    const { pendingDelete } = this.state;
    if (!pendingDelete) return;
    const target = pendingDelete;
    this.setState({ deleteError: null, deleting: true });
    try {
      await wsRequest("delete_session", { sessionID: target.id });
      if (this.disposed) return;
      this.setState({ deleting: false });
      removeSession(target.id);
      // Decide against the store's CURRENT active session, not the one
      // captured when the request was sent (task #694): the confirm
      // dialog's overlay blocks row clicks, but live events still switch
      // the active session while the reply is in flight (session_created
      // auto-focus, hashchange, sessions_list routing). Clearing from a
      // stale capture would wipe the now-active session's transcript and
      // hash for a delete that no longer concerns it.
      if (sessionsCarburetor.getData().activeSessionID === target.id) setActiveSession(null);
      this.setState({ pendingDelete: null });
    } catch (e) {
      if (this.disposed) return;
      this.setState({ deleting: false, deleteError: e instanceof Error ? e.message : String(e) });
    }
  }

  /** Opens the delete-all-others confirmation. */
  @bind
  protected openDeleteOthers(): void {
    this.setState({ deleteOthersError: null, confirmDeleteOthers: true });
  }

  /** Closes the delete-all-others confirmation and forgets its error. */
  @bind
  protected cancelDeleteOthers(): void {
    this.setState({ confirmDeleteOthers: false, deleteOthersError: null });
  }

  /** Deletes every session except the active one. */
  @bind
  protected async confirmDeleteOtherSessions(): Promise<void> {
    const activeID = sessionsCarburetor.getData().activeSessionID;
    if (!activeID) return;
    this.setState({ deleteOthersError: null, deletingOthers: true });
    try {
      const msg = await wsRequest<DeleteOtherSessionsResult>("delete_other_sessions", { keepID: activeID });
      if (this.disposed) return;
      this.setState({ deletingOthers: false });
      // Only drop rows the server actually confirms deleted (task #684) —
      // a partial failure (deletedIDs missing some non-kept session) must
      // leave the survivor's row in place, not vanish on a blanket "ok".
      // Reconcile against the store's CURRENT session list and active
      // session, not the ones captured when the request was sent (task
      // #694): a session created while the reply was in flight can be
      // deleted server-side and must not survive as a ghost row, and the
      // active session can be switched mid-flight onto an ID the server
      // then deleted — leaving it active would dangle. The kept session
      // never appears in deletedIDs, so membership alone filters rows;
      // mirroring useWS's session_deleted handler, a deleted session that
      // is still the active one clears the active session.
      const result = msg.payload;
      const deletedIDs = new Set(result?.deletedIDs ?? []);
      for (const s of sessionsCarburetor.getData().sessions) {
        if (!deletedIDs.has(s.ID)) continue;
        removeSession(s.ID);
        if (sessionsCarburetor.getData().activeSessionID === s.ID) setActiveSession(null);
      }
      if (result && result.failedIDs.length > 0) {
        this.setState({
          deleteOthersError:
            `${result.failedIDs.length} session${result.failedIDs.length === 1 ? "" : "s"} could not be deleted`,
        });
        return;
      }
      this.setState({ confirmDeleteOthers: false });
    } catch (e) {
      if (this.disposed) return;
      this.setState({ deletingOthers: false, deleteOthersError: e instanceof Error ? e.message : String(e) });
    }
  }

  /**
   * Starts renaming a session.
   *
   * @param id - the session id
   * @param title - its current title
   */
  @bind
  protected startEditing(id: string, title: string): void {
    this.setState({ editingID: id, editTitle: title || "Untitled session" });
  }

  /**
   * Stores the typed rename draft.
   *
   * @param editTitle - the draft
   */
  @bind
  protected onEditTitleChange(editTitle: string): void {
    this.setState({ editTitle });
  }

  /** Sends the rename and closes the editor. */
  @bind
  protected saveRename(): void {
    const { editingID, editTitle } = this.state;
    if (editingID) {
      ws.send("rename_session", { sessionID: editingID, title: editTitle.trim() || "Untitled session" });
      this.setState({ editingID: null });
    }
  }

  /** Closes the rename editor without saving. */
  @bind
  protected cancelEditing(): void {
    this.setState({ editingID: null });
  }

  /**
   * Enter saves the rename, Escape cancels it.
   *
   * @param e - the keydown event
   */
  @bind
  protected handleKeyDown(e: KeyboardEvent): void {
    if (e.key === "Enter") {
      this.saveRename();
    } else if (e.key === "Escape") {
      this.setState({ editingID: null });
    }
  }

  /**
   * Renders one session row.
   *
   * @param s - the session
   */
  @bind
  protected renderSession(s: TReadonly<Session>) {
    const { editingID, editTitle } = this.state;
    const isEditing = editingID === s.ID;
    return (
      <SessionRow
        key={s.ID}
        session={s}
        isEditing={isEditing}
        editTitle={isEditing ? editTitle : ""}
        onSelect={this.selectSession}
        onStartEdit={this.startEditing}
        onDelete={this.deleteSession}
        onEditTitleChange={this.onEditTitleChange}
        onEditKeyDown={this.handleKeyDown}
        onSaveRename={this.saveRename}
        onCancelEdit={this.cancelEditing}
      />
    );
  }

  /** Renders the sidebar. */
  public render() {
    const { pendingDelete, deleteError, deleting, confirmDeleteOthers, deleteOthersError, deletingOthers } = this.state;
    const sessions = this.sessionsView().filter(isTopLevel);
    const { activeSessionID: activeID } = this.useCarburetor(sessionsCarburetor);
    const cwd = this.useCarburetor(configCarburetor).config?.cwd;

    return (
      <aside
        className="w-64 bg-base-subtle border-r border-surface flex flex-col overflow-hidden shrink-0"
        data-test-id="sidebar"
      >
        {/* Header */}
        <div
          className="flex items-center justify-between px-6 py-5 border-b border-surface"
          data-test-id="sidebar-header"
        >
          <div className="flex flex-col gap-0.5">
            <span className="text-xl font-black text-accent tracking-tighter">Rush~</span>
            <span className="text-[10px] text-text-subtle font-mono opacity-60">
              #{__GIT_COUNT__} · {__GIT_COMMIT__}
            </span>
            <span className="text-[10px] text-text-subtle font-mono opacity-50">{__GIT_BRANCH__}</span>
          </div>
          <div className="flex items-center gap-1.5">
            <button
              onClick={this.newSession}
              title="New session"
              data-test-id="sidebar-new-session"
              className="flex items-center gap-2 px-4 py-2 bg-accent-fill text-white/90 text-sm font-bold rounded-xl hover:bg-accent/90 active:scale-95 transition-all shadow-sm"
            >
              <Plus size={18} />
              New
            </button>
            <button
              onClick={this.openDeleteOthers}
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
        {cwd && (
          <div
            className="flex items-center gap-1.5 px-6 py-2 border-t border-surface/50 text-[11px] text-text-subtle font-mono truncate"
            title={cwd}
          >
            <Folder size={11} className="shrink-0" />
            <span className="truncate">{cwd}</span>
          </div>
        )}

        {/* Session list */}
        <div className="flex-1 overflow-y-auto py-3 px-3" data-test-id="sidebar-session-list">
          {sessions.length === 0 ? (
            <div
              className="flex flex-col items-center justify-center py-16 px-6 text-center"
              data-test-id="sidebar-empty"
            >
              <div className="w-12 h-12 rounded-2xl bg-base-overlay flex items-center justify-center mb-4 text-text-subtle">
                <MessageSquare size={24} />
              </div>
              <p className="text-text-muted text-base font-semibold">No sessions yet</p>
              <p className="text-text-subtle text-sm mt-1.5">Click New to get started</p>
            </div>
          ) : (
            sessions.map(this.renderSession)
          )}
        </div>

        {pendingDelete && (
          <ConfirmDialog
            title="Delete session"
            message={`"${pendingDelete.title}" and all its messages will be permanently deleted.`}
            confirmLabel={deleting ? "Deleting…" : "Delete"}
            onConfirm={this.confirmDelete}
            onCancel={this.cancelDelete}
            error={deleteError}
            busy={deleting}
          />
        )}

        {confirmDeleteOthers && (
          <ConfirmDialog
            title="Delete all other sessions"
            message="Delete all sessions except the current one? This cannot be undone."
            confirmLabel={deletingOthers ? "Deleting…" : "Delete all"}
            onConfirm={this.confirmDeleteOtherSessions}
            onCancel={this.cancelDeleteOthers}
            error={deleteOthersError}
            busy={deletingOthers}
          />
        )}
      </aside>
    );
  }
}
