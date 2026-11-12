import { createRef } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { FileText, MoreHorizontal, Plug, PowerOff, ScrollText, ServerCog, Settings, SlidersHorizontal } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import type { ToolbarModal } from "./ToolbarModal";

/** Props of one menu row. */
interface IMoreMenuItemProps {
  kind: ToolbarModal;
  testId: string;
  title: string;
  label: string;
  Icon: LucideIcon;
  danger?: boolean;
  onPick: (kind: ToolbarModal) => void;
}

/** One row of the More dropdown. */
class MoreMenuItem extends AntiHookComponent<IMoreMenuItemProps> {
  /** Reports this row's modal as picked. */
  @bind
  protected pick(): void {
    this.props.onPick(this.props.kind);
  }

  /** Renders the row. */
  public render() {
    const { testId, title, label, Icon, danger } = this.props;
    const tone = danger
      ? "text-red hover:bg-red/10"
      : "text-text-subtle hover:bg-base-overlay hover:text-text";
    return (
      <button
        data-test-id={testId}
        onClick={this.pick}
        className={`w-full text-left px-3 py-2 flex items-center gap-2 text-sm ${tone} transition-colors`}
        title={title}
      >
        <Icon size={14} />
        <span>{label}</span>
      </button>
    );
  }
}

/** Props of the More menu. */
interface IMoreMenuProps {
  open: boolean;
  /** Prompt needs an active session. */
  hasSession: boolean;
  onToggle: () => void;
  onClose: () => void;
  /** Called with the modal the operator picked; the menu closes itself first. */
  onPick: (kind: ToolbarModal) => void;
}

/** More dropdown for rarely-used buttons: prompt, MCP, providers, default models, settings, logs, shutdown. */
export class MoreMenu extends AntiHookComponent<IMoreMenuProps> {
  private readonly menuRef = createRef<HTMLDivElement>();

  /** Attaches the dismiss listeners while the menu is open. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.bindDismiss, [this.props.open]);
  }

  /** Arms outside-click and Escape dismissal; returns the disarm function. */
  @bind
  protected bindDismiss(): (() => void) | undefined {
    if (!this.props.open) return undefined;
    document.addEventListener("mousedown", this.handleOutsideClick);
    document.addEventListener("keydown", this.handleEscape);
    return this.unbindDismiss;
  }

  /** Disarms the dismiss listeners. */
  @bind
  protected unbindDismiss(): void {
    document.removeEventListener("mousedown", this.handleOutsideClick);
    document.removeEventListener("keydown", this.handleEscape);
  }

  /**
   * Closes the menu on a click outside it.
   *
   * @param e - the mousedown event
   */
  @bind
  protected handleOutsideClick(e: MouseEvent): void {
    if (this.menuRef.current && !this.menuRef.current.contains(e.target as Node)) {
      this.props.onClose();
    }
  }

  /**
   * Closes the menu on Escape.
   *
   * @param e - the keydown event
   */
  @bind
  protected handleEscape(e: KeyboardEvent): void {
    if (e.key === "Escape") {
      this.props.onClose();
    }
  }

  /**
   * Closes the menu, then reports the pick.
   *
   * @param kind - the picked modal
   */
  @bind
  protected pick(kind: ToolbarModal): void {
    this.props.onClose();
    this.props.onPick(kind);
  }

  /** Renders the trigger and, while open, the dropdown. */
  public render() {
    const { open, hasSession, onToggle } = this.props;
    return (
      <div ref={this.menuRef} className="relative">
        <button
          data-test-id="header-more-button"
          onClick={onToggle}
          title="More — prompt, MCP, providers, default models, settings, logs"
          className={
            "flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors " +
            "bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
          }
        >
          <MoreHorizontal size={13} />
          <span>More</span>
        </button>

        {open && (
          <div className="absolute bottom-full mb-2 left-0 w-52 bg-canvas border border-surface rounded-xl shadow-xl z-50 py-1">
            {hasSession && (
              <MoreMenuItem
                kind="prompt"
                testId="header-prompt-button"
                title="View / edit system prompt"
                label="Prompt"
                Icon={ScrollText}
                onPick={this.pick}
              />
            )}
            <MoreMenuItem
              kind="mcp"
              testId="header-mcp-button"
              title="MCP server settings"
              label="MCP"
              Icon={Plug}
              onPick={this.pick}
            />
            <MoreMenuItem
              kind="providers"
              testId="header-providers-button"
              title="Custom providers"
              label="Providers"
              Icon={ServerCog}
              onPick={this.pick}
            />
            <MoreMenuItem
              kind="scoped"
              testId="header-default-models-button"
              title="Default models — System / Folder / Session"
              label="Default models"
              Icon={SlidersHorizontal}
              onPick={this.pick}
            />
            <MoreMenuItem
              kind="settings"
              testId="header-settings-button"
              title="Settings"
              label="Settings"
              Icon={Settings}
              onPick={this.pick}
            />
            <MoreMenuItem
              kind="logs"
              testId="header-logs-button"
              title="View logs"
              label="Logs"
              Icon={FileText}
              onPick={this.pick}
            />
            <div className="border-t border-surface mt-1 pt-1">
              <MoreMenuItem
                kind="shutdown"
                testId="header-shutdown-button"
                title="Shut down the whole server process"
                label="Shut down server"
                Icon={PowerOff}
                danger
                onPick={this.pick}
              />
            </div>
          </div>
        )}
      </div>
    );
  }
}
