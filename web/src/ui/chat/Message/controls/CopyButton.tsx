// Copy-to-clipboard button with a "Copied" confirmation flash.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Check, Copy } from "lucide-react";

/** Props of the copy button. */
interface ICopyButtonProps {
  text: string;
  className?: string;
  label?: string;
}

/** Local state of the copy button. */
interface ICopyButtonState {
  copied: boolean;
}

/** Copies its text to the clipboard and flashes "Copied" for 1.5 s. */
export class CopyButton extends AntiHookComponent<ICopyButtonProps, ICopyButtonState> {
  public state: ICopyButtonState = { copied: false };

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

  /** Writes the text to the clipboard, then flashes the confirmation. */
  @bind
  protected copy(): void {
    void navigator.clipboard.writeText(this.props.text).then(this.flash);
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
    const { className = "", label = "Copy" } = this.props;
    const { copied } = this.state;
    return (
      <button onClick={this.copy} title={label} className={`btn-copy ${className}`}>
        {copied ? (
          <>
            <Check size={14} className="text-green" />
            <span className="text-green">Copied</span>
          </>
        ) : (
          <>
            <Copy size={14} />
            <span>{label}</span>
          </>
        )}
      </button>
    );
  }
}
