import type { ChangeEvent, MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { wsRequest } from "../../ws";
import { SystemPromptFetch } from "./SystemPromptFetch";

/** Props of the system-prompt modal. */
interface ISystemPromptModalProps {
  sessionID: string;
  onClose: () => void;
}

/** Local state of the system-prompt modal. */
interface ISystemPromptModalState {
  original: string;
  draft: string;
  loading: boolean;
  saving: boolean;
  error: string | null;
}

/** View / edit the session's system prompt. */
export class SystemPromptModal extends AntiHookComponent<ISystemPromptModalProps, ISystemPromptModalState> {
  public state: ISystemPromptModalState = { original: "", draft: "", loading: true, saving: false, error: null };

  /**
   * Set when the modal unmounts: save()'s continuation checks it after
   * its await so a reply landing after close cannot setState on a dead
   * modal (the wsRequest listeners detach themselves on settle).
   */
  private disposed = false;

  private fetch: SystemPromptFetch | undefined;

  /** Declares the mount flag and the per-session fetch. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.markMounted, []);
    this.useEffect(this.startSession, [this.props.sessionID, this.props.onClose]);
  }

  /** Clears the disposed flag on mount; the cleanup sets it on unmount. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Flags the modal as gone. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  /**
   * Fresh fetch for this session, plus the Escape handler.
   *
   * The modal stays mounted while the active session can flip underneath it (a broadcast
   * session_created from another client re-renders us with a new sessionID prop), so any
   * state carried over from the previous session is reset — otherwise content fetched for
   * session A could be saved into session B (review F5, task #693).
   */
  @bind
  protected startSession(): () => void {
    const { original, draft, loading, error } = this.state;
    if (!loading || error !== null || original !== "" || draft !== "") {
      this.setState({ loading: true, error: null, original: "", draft: "" });
    }
    this.fetch = new SystemPromptFetch(this.props.sessionID, {
      loaded: this.onLoaded,
      failed: this.onFailed,
    });
    this.fetch.start();
    document.addEventListener("keydown", this.onKey);
    return this.endSession;
  }

  /** Cancels the fetch and detaches the Escape handler. */
  @bind
  protected endSession(): void {
    this.fetch?.dispose();
    this.fetch = undefined;
    document.removeEventListener("keydown", this.onKey);
  }

  /**
   * The fetched prompt arrived: show it and make it the saved baseline.
   *
   * @param content - the prompt text
   */
  @bind
  protected onLoaded(content: string): void {
    this.setState({ loading: false, original: content, draft: content });
  }

  /**
   * The fetch failed: show why.
   *
   * @param message - the failure text
   */
  @bind
  protected onFailed(message: string): void {
    this.setState({ loading: false, error: message });
  }

  /**
   * Escape closes the modal.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    if (e.key === "Escape") this.props.onClose();
  }

  /** Saves the draft; a reply landing after unmount is ignored. */
  @bind
  protected async save(): Promise<void> {
    this.setState({ error: null, saving: true });
    const savedDraft = this.state.draft;
    try {
      await wsRequest("set_system_prompt", { sessionID: this.props.sessionID, content: savedDraft });
      if (this.disposed) return;
      this.setState({ saving: false, original: savedDraft });
    } catch (e) {
      if (this.disposed) return;
      this.setState({ saving: false, error: e instanceof Error ? e.message : String(e) });
    }
  }

  /** Drops unsaved edits. */
  @bind
  protected reset(): void {
    this.setState({ draft: this.state.original });
  }

  /**
   * Tracks the edited text.
   *
   * @param e - the change event
   */
  @bind
  protected onDraftChange(e: ChangeEvent<HTMLTextAreaElement>): void {
    this.setState({ draft: e.target.value });
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

  /** Renders the editor body: the loading note or the textarea. */
  protected renderBody() {
    if (this.state.loading) {
      return <p className="text-text-subtle text-sm p-2">Loading…</p>;
    }
    return (
      <textarea
        className={
          "w-full h-full min-h-[400px] text-xs font-mono text-text bg-base-overlay border border-surface " +
          "rounded-xl p-3 resize-none outline-none focus:border-accent/50 leading-relaxed"
        }
        value={this.state.draft}
        onChange={this.onDraftChange}
        spellCheck={false}
      />
    );
  }

  /** Renders the modal. */
  public render() {
    const { original, draft, saving, error } = this.state;
    const dirty = draft !== original;
    return (
      <div
        className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm"
        onClick={this.props.onClose}
      >
        <div
          className={
            "bg-canvas border border-surface rounded-2xl shadow-xl flex flex-col w-full max-w-3xl mx-4 max-h-[85vh]"
          }
          onClick={this.stopPropagation}
        >
          <div className="flex items-center justify-between px-6 py-4 border-b border-surface shrink-0">
            <h2 className="text-base font-semibold text-text">System Prompt</h2>
            <button
              onClick={this.props.onClose}
              className="text-text-subtle hover:text-text transition-colors text-xl leading-none"
            >
              ×
            </button>
          </div>
          <div className="flex-1 overflow-hidden p-4">{this.renderBody()}</div>
          <div className="flex items-center justify-end gap-3 px-6 py-4 border-t border-surface shrink-0">
            {error && (
              <span className="flex-1 text-sm text-red truncate" title={error}>
                {error}
              </span>
            )}
            {dirty && (
              <button
                onClick={this.reset}
                className={
                  "px-4 py-2 text-sm text-text-subtle hover:text-text transition-colors rounded-xl " +
                  "hover:bg-base-overlay"
                }
              >
                Reset
              </button>
            )}
            <button
              onClick={this.save}
              disabled={!dirty || saving}
              className={
                "px-4 py-2 text-sm font-medium bg-accent-fill text-white/90 rounded-xl hover:opacity-90 " +
                "transition-opacity disabled:opacity-40 disabled:cursor-not-allowed"
              }
            >
              Save
            </button>
          </div>
        </div>
      </div>
    );
  }
}
