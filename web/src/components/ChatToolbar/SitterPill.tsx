import { AntiHookComponent } from "@react-carburetor/core";
import { Eye, X } from "lucide-react";
import { sitterLoop } from "../../sitter/sitterLoop";
import { sitterCarburetor } from "../../state/sitter";

/** Disarms the sitter loop. */
const stopSitter = (): void => {
  sitterLoop.stop();
};

/**
 * Sitter pill: visible only while the auto-resume loop is armed.
 * Shows the session it is watching (initials from the title) and
 * the wake interval. Click to disarm. Toggled from the chat input
 * via `/sitter [N]`.
 */
export class SitterPill extends AntiHookComponent {
  /** Renders the pill, or nothing while the loop is idle. */
  public render() {
    const { sitter } = this.useCarburetor(sitterCarburetor);
    if (!sitter.running) return null;
    return (
      <button
        data-test-id="header-sitter-pill"
        onClick={stopSitter}
        title={
          `Sitter armed — checking session every ${sitter.intervalMin}m. Resumes the agent if it stalls AND ` +
          "there are open todos. Click to disable. (Or type /sitter in the chat.)"
        }
        className={
          "flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border bg-accent/10 " +
          "border-accent/40 text-accent hover:bg-red/10 hover:border-red/40 hover:text-red transition-colors"
        }
      >
        <Eye size={13} />
        <span>Sitter {sitter.intervalMin}m</span>
        <X size={11} className="opacity-60" />
      </button>
    );
  }
}
