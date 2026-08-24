import { useEffect } from "react";
import { AlertTriangle, Trash2, X } from "lucide-react";

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

export function ConfirmDialog({
  title,
  message,
  confirmLabel = "Delete",
  variant = "danger",
  onConfirm,
  onCancel,
  error,
  busy = false,
}: Props) {
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (busy) return;
      if (e.key === "Escape") onCancel();
      if (e.key === "Enter") { e.preventDefault(); onConfirm(); }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onConfirm, onCancel, busy]);

  const isDanger = variant === "danger";

  return (
    <div
      className="modal-overlay p-4 z-[100]"
      onClick={() => {
        // Same busy guard as the buttons and the key handler: a confirm
        // in flight must not be dismissed, only concluded or errored.
        if (!busy) onCancel();
      }}
    >
      <div
        className="modal-panel w-full max-w-sm overflow-hidden chat-font"
        onClick={(e) => e.stopPropagation()}
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
            className={`px-4 py-2 text-sm font-semibold text-white/90 rounded-xl transition-all active:scale-[0.97] shadow-sm disabled:opacity-40 disabled:cursor-not-allowed ${
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
