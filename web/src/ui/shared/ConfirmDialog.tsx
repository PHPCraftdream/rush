import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { AlertTriangle, Trash2, X } from "lucide-react";

/** Props of the confirmation dialog. */
interface Props {
  title: string;
  message: string;
  confirmLabel?: string;
  /** "danger" = red button (default), "warning" = yellow */
  variant?: "danger" | "warning";
  onConfirm: () => void;
  onCancel: () => void;
  /**
   * Inline error from a rejected confirm (task #684): shown inside the
   * still-open dialog instead of relying solely on the global transcript-pane
   * banner, mirroring SystemPromptModal's inline error (ChatToolbar.tsx).
   */
  error?: string | null;
  /** Disables both buttons and suppresses Enter/Escape while a confirm is in flight. */
  busy?: boolean;
}

/** Classes of the confirm button shared by both variants. */
const CONFIRM_BASE =
  "px-4 py-2 text-sm font-semibold text-white/90 rounded-xl transition-all active:scale-[0.97] shadow-sm " +
  "disabled:opacity-40 disabled:cursor-not-allowed";

/** Modal confirm/cancel dialog; Enter confirms and Escape cancels unless busy. */
export class ConfirmDialog extends AntiHookComponent<Props> {
  /** Subscribes the keyboard handler; re-subscribes when its inputs change. */
  protected useEffects(): void {
    super.useEffects();
    const { onConfirm, onCancel, busy = false } = this.props;
    this.useEffect(this.bindKeyHandler, [onConfirm, onCancel, busy]);
  }

  /** Attaches the document key handler; returns its detach function. */
  @bind
  protected bindKeyHandler(): () => void {
    document.addEventListener("keydown", this.onKey);
    return this.unbindKeyHandler;
  }

  /** Detaches the document key handler. */
  @bind
  protected unbindKeyHandler(): void {
    document.removeEventListener("keydown", this.onKey);
  }

  /**
   * Escape cancels, Enter confirms; both are ignored while a confirm is in flight.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    const { onConfirm, onCancel, busy = false } = this.props;
    if (busy) return;
    if (e.key === "Escape") onCancel();
    if (e.key === "Enter") { e.preventDefault(); onConfirm(); }
  }

  /** Backdrop click cancels, with the same busy guard as the buttons and the key handler. */
  @bind
  protected onOverlayClick(): void {
    // A confirm in flight must not be dismissed, only concluded or errored.
    if (!this.props.busy) this.props.onCancel();
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

  /** Renders the dialog. */
  public render() {
    const {
      title,
      message,
      confirmLabel = "Delete",
      variant = "danger",
      onConfirm,
      onCancel,
      error,
      busy = false,
    } = this.props;
    const isDanger = variant === "danger";

    return (
      <div
        className="modal-overlay p-4 z-[100]"
        onClick={this.onOverlayClick}
      >
        <div
          data-test-id="confirm-dialog"
          className="modal-panel w-full max-w-sm overflow-hidden chat-font"
          onClick={this.stopPropagation}
        >
          {/* icon strip */}
          <div className={`px-6 pt-6 pb-4 flex items-start gap-4`}>
            <div className={`shrink-0 w-10 h-10 rounded-xl flex items-center justify-center ${
              isDanger ? "bg-red/10 text-red" : "bg-yellow/10 text-yellow"
            }`}>
              {isDanger ? <Trash2 size={20} /> : <AlertTriangle size={20} />}
            </div>
            <div className="flex-1 min-w-0">
              <h3 className="text-[15px] font-semibold text-text leading-snug">{title}</h3>
              <p className="text-sm text-text-muted mt-1 leading-relaxed">{message}</p>
            </div>
            <button
              onClick={onCancel}
              disabled={busy}
              className="shrink-0 p-1 rounded-lg text-text-subtle hover:text-text hover:bg-base-overlay transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
            >
              <X size={16} />
            </button>
          </div>

          {error && (
            <p className="px-6 pb-4 text-sm text-red leading-relaxed" data-test-id="confirm-dialog-error">
              {error}
            </p>
          )}

          {/* actions */}
          <div className="flex items-center justify-end gap-2 px-6 py-4 border-t border-surface bg-base-subtle">
            <button
              onClick={onCancel}
              disabled={busy}
              className="px-4 py-2 text-sm font-medium text-text-muted hover:text-text hover:bg-base-overlay rounded-xl transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
            >
              Cancel
            </button>
            <button
              onClick={onConfirm}
              disabled={busy}
              className={`${CONFIRM_BASE} ${
                isDanger
                  ? "bg-red-fill hover:opacity-90 shadow-red/20"
                  : "bg-yellow-fill hover:opacity-90 shadow-yellow/20"
              }`}
            >
              {confirmLabel}
            </button>
          </div>
        </div>
      </div>
    );
  }
}
