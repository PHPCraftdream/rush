import { createRef } from "react";
import type { ChangeEvent, KeyboardEvent, MouseEvent } from "react";
import { createPortal } from "react-dom";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { GitFork } from "lucide-react";
import { ws } from "../ws";

/** Props of the fork-session dialog. */
interface IForkSessionModalProps {
  sessionID: string;
  defaultTitle: string;
  onClose: () => void;
}

/** Local state of the fork-session dialog. */
interface IForkSessionModalState {
  title: string;
}

/** Modal asking for the new session's name before forking the current one. */
export class ForkSessionModal extends AntiHookComponent<IForkSessionModalProps, IForkSessionModalState> {
  public state: IForkSessionModalState = { title: this.props.defaultTitle };

  private readonly inputRef = createRef<HTMLInputElement>();

  /** Focuses and selects the name field once, on mount. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("focusInput", this.focusInput, []);
  }

  /** Focuses the name field and selects its text. */
  @bind
  protected focusInput(): void {
    this.inputRef.current?.focus();
    this.inputRef.current?.select();
  }

  /** Sends the fork request (falling back to the default title when blank) and closes. */
  @bind
  protected confirm(): void {
    const { sessionID, defaultTitle, onClose } = this.props;
    const t = this.state.title.trim() || defaultTitle;
    ws.send("fork_session", { sessionID, title: t });
    onClose();
  }

  /**
   * Enter confirms, Escape closes.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent<HTMLInputElement>): void {
    if (e.key === "Enter") this.confirm();
    if (e.key === "Escape") this.props.onClose();
  }

  /**
   * Stores the typed title.
   *
   * @param e - the input change event
   */
  @bind
  protected onTitleChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ title: e.target.value });
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

  /** Renders the dialog into document.body. */
  public render() {
    const { onClose } = this.props;
    return createPortal(
      <div className="modal-overlay z-[9999]" onClick={onClose}>
        <div className="modal-panel chat-font p-6 w-full max-w-md mx-4" onClick={this.stopPropagation}>
          <div className="flex items-center gap-2.5 mb-5">
            <GitFork size={18} className="text-accent shrink-0" />
            <h2 className="font-bold text-text">Fork session</h2>
          </div>
          <label className="field-label">
            Name for the new session
          </label>
          <input
            ref={this.inputRef}
            value={this.state.title}
            onChange={this.onTitleChange}
            onKeyDown={this.onKey}
            className="field-input"
            style={{ fontSize: "var(--chat-font-size)" }}
          />
          <div className="modal-footer mt-5">
            <button onClick={onClose} className="btn-cancel text-sm">
              Cancel
            </button>
            <button
              onClick={this.confirm}
              className="px-4 py-2 rounded-xl btn-primary text-sm flex items-center gap-1.5"
            >
              <GitFork size={14} />
              Fork
            </button>
          </div>
        </div>
      </div>,
      document.body
    );
  }
}
