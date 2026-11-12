import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Minimize2, X } from "lucide-react";
import { cancelQueuedSummarize, summarizeSession } from "../../store";

/** Props of the Compact button. */
interface ICompactButtonProps {
  sessionID: string;
  isQueued: boolean;
  isBusy: boolean;
  hasMessages: boolean;
}

/** Compact — operates on the active session's history; while queued it cancels the request. */
export class CompactButton extends AntiHookComponent<ICompactButtonProps> {
  /** Cancels the queued compact request. */
  @bind
  protected cancel(): void {
    cancelQueuedSummarize(this.props.sessionID);
  }

  /** Compacts the session's history. */
  @bind
  protected compact(): void {
    summarizeSession(this.props.sessionID);
  }

  /** Renders the queued or the idle variant. */
  public render() {
    const { isQueued, isBusy, hasMessages } = this.props;
    if (isQueued) {
      return (
        <button
          onClick={this.cancel}
          title="Compact is queued — click to cancel"
          className={
            "btn-toolbar text-accent border-accent/30 bg-accent/5 hover:bg-red/10 hover:text-red " +
            "hover:border-red/30 flex items-center gap-1"
          }
        >
          <Minimize2 size={13} />
          Compact queued
          <X size={11} className="opacity-60" />
        </button>
      );
    }
    return (
      <button
        onClick={this.compact}
        disabled={!hasMessages}
        title={
          isBusy
            ? "Compact will run after the current task finishes"
            : "Compact — compress conversation history to free up context window"
        }
        className="btn-toolbar"
      >
        <Minimize2 size={13} />
        Compact
      </button>
    );
  }
}
