// Context menu of the selection checkbox: "select all above / below".

import { createRef } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { ChevronsUp, ChevronsDown } from "lucide-react";

/** Props of the checkbox context menu. */
interface ICheckboxContextMenuProps {
  x: number;
  y: number;
  onSelectAbove: () => void;
  onSelectBelow: () => void;
  onClose: () => void;
}

/** Floating menu at the click point; closes on an outside press or Escape. */
export class CheckboxContextMenu extends AntiHookComponent<ICheckboxContextMenuProps> {
  private readonly menuRef = createRef<HTMLDivElement>();

  /** Attaches the document listeners; re-attaches when the close callback changes. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.bindListeners, [this.props.onClose]);
  }

  /** Attaches the outside-press and Escape listeners; returns their detach function. */
  @bind
  protected bindListeners(): () => void {
    document.addEventListener("mousedown", this.onMouseDown);
    document.addEventListener("keydown", this.onKey);
    return this.unbindListeners;
  }

  /** Detaches the outside-press and Escape listeners. */
  @bind
  protected unbindListeners(): void {
    document.removeEventListener("mousedown", this.onMouseDown);
    document.removeEventListener("keydown", this.onKey);
  }

  /**
   * Closes the menu on a press outside it.
   *
   * @param e - the mousedown event
   */
  @bind
  protected onMouseDown(e: MouseEvent): void {
    const menu = this.menuRef.current;
    if (menu && !menu.contains(e.target as Node)) this.props.onClose();
  }

  /**
   * Closes the menu on Escape.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    if (e.key === "Escape") this.props.onClose();
  }

  /** Renders the menu. */
  public render() {
    const { x, y, onSelectAbove, onSelectBelow } = this.props;
    return (
      <div
        ref={this.menuRef}
        className="fixed z-[9999] min-w-[200px] py-1 rounded-xl border border-surface bg-canvas shadow-xl chat-font"
        style={{ left: x, top: y }}
      >
        <button
          onClick={onSelectAbove}
          className="w-full flex items-center gap-2 px-3 py-1.5 text-sm text-text hover:bg-base-overlay transition-colors"
        >
          <ChevronsUp size={14} className="text-text-subtle" />
          + Select all above
        </button>
        <button
          onClick={onSelectBelow}
          className="w-full flex items-center gap-2 px-3 py-1.5 text-sm text-text hover:bg-base-overlay transition-colors"
        >
          <ChevronsDown size={14} className="text-text-subtle" />
          + Select all below
        </button>
      </div>
    );
  }
}
