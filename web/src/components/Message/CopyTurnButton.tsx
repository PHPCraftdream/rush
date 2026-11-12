// Copies the agent's full turn (thinking + every text reply) for any message
// ID in the turn.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Check, Copy } from "lucide-react";
import { collectTurnContent } from "../../store";

/** Props of the copy-turn button. */
interface ICopyTurnButtonProps {
  messageID: string;
}

/** Local state of the copy-turn button. */
interface ICopyTurnButtonState {
  copied: boolean;
}

/**
 * Copies the agent's full prose response to one user prompt — thinking + all
 * intermediate text + final text, across every assistant message until the next
 * user turn. Content is gathered LAZILY on click so a long streaming turn
 * doesn't rebuild this string on every delta.
 *
 * Accepts ANY message ID belonging to the turn: a user prompt, an intermediate
 * assistant step, or the final assistant message. collectTurnContent walks back
 * to the turn's user message either way.
 */
export class CopyTurnButton extends AntiHookComponent<ICopyTurnButtonProps, ICopyTurnButtonState> {
  public state: ICopyTurnButtonState = { copied: false };

  private flashTimer: ReturnType<typeof setTimeout> | undefined;

  /** Arms the teardown that cancels a pending flash reset on unmount. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.armTimerCleanup, []);
  }

  /** Returns the cleanup that cancels a pending flash reset. */
  @bind
  protected armTimerCleanup(): () => void {
    return this.cancelFlashTimer;
  }

  /** Cancels a pending flash reset. */
  @bind
  protected cancelFlashTimer(): void {
    clearTimeout(this.flashTimer);
  }

  /** Gathers the turn's text on demand, copies it, then flashes the confirmation. */
  @bind
  protected copy(): void {
    const text = collectTurnContent(this.props.messageID);
    if (!text) return;
    void navigator.clipboard.writeText(text).then(this.flash);
  }

  /** Shows the confirmation and schedules its reset. */
  @bind
  protected flash(): void {
    this.setState({ copied: true });
    this.flashTimer = setTimeout(this.resetFlash, 1500);
  }

  /** Hides the confirmation. */
  @bind
  protected resetFlash(): void {
    this.setState({ copied: false });
  }

  /** Renders the button. */
  public render() {
    const { copied } = this.state;
    return (
      <button onClick={this.copy} title="Copy all (thinking + every text reply, no tools)" className="btn-copy">
        {copied ? (
          <>
            <Check size={14} className="text-green" />
            <span className="text-green">Copied</span>
          </>
        ) : (
          <>
            <Copy size={14} />
            <span>Copy all</span>
          </>
        )}
      </button>
    );
  }
}
