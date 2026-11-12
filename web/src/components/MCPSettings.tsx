import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { mcpCarburetor } from "../state/mcp";
import type { IMcpState } from "../state/McpCarburetor";
import { ws, wsRequest } from "../ws";
import { X, Plus } from "lucide-react";
import type { MCPServerInfo } from "../types";
import { MCPForm } from "./mcp/MCPForm";
import { ServerRow } from "./mcp/ServerRow";
import { mcpServerPayload } from "./mcp/mcpServerPayload";

/** Shared empty server list for a missing snapshot. */
const NO_SERVERS: TReadonly<MCPServerInfo[]> = [];

/**
 * Picks the server list out of the MCP state.
 *
 * @param state - the MCP state
 */
const selectServers = (state: TReadonly<IMcpState>) => state.mcp?.servers ?? NO_SERVERS;

/**
 * Orders servers by name.
 *
 * @param a - first server
 * @param b - second server
 */
const byName = (a: TReadonly<MCPServerInfo>, b: TReadonly<MCPServerInfo>): number => a.name.localeCompare(b.name);

/**
 * Whether a server is enabled.
 *
 * @param s - the server
 */
const isEnabled = (s: TReadonly<MCPServerInfo>): boolean => !s.disabled;

// ── MCP Settings Modal ────────────────────────────────────────────────────────

/** Props of the MCP settings dialog. */
interface IMCPSettingsProps {
  onClose: () => void;
}

/** Local state of the MCP settings dialog. */
interface IMCPSettingsState {
  showAdd: boolean;
}

/** Dialog listing the MCP servers, with add/edit/enable/remove. */
export class MCPSettings extends AntiHookComponent<IMCPSettingsProps, IMCPSettingsState> {
  public state: IMCPSettingsState = { showAdd: false };

  /** Detached snapshot of the server list, safe to hand to rows. */
  private readonly serversView = this.connectSelection(mcpCarburetor, selectServers);

  /** Closes the dialog on Escape while mounted. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("escapeKey", this.bindEscape, [this.props.onClose]);
  }

  /** Attaches the Escape handler; returns its detach function. */
  @bind
  protected bindEscape(): () => void {
    document.addEventListener("keydown", this.onKey);
    return this.unbindEscape;
  }

  /** Detaches the Escape handler. */
  @bind
  protected unbindEscape(): void {
    document.removeEventListener("keydown", this.onKey);
  }

  /**
   * Closes the dialog on Escape.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    if (e.key === "Escape") this.props.onClose();
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

  /** Opens the add form. */
  @bind
  protected openAdd(): void {
    this.setState({ showAdd: true });
  }

  /** Closes the add form. */
  @bind
  protected closeAdd(): void {
    this.setState({ showAdd: false });
  }

  /**
   * Removes a server.
   *
   * @param name - the server name
   */
  @bind
  protected removeServer(name: string): void {
    ws.send("remove_mcp_server", { name });
  }

  /**
   * Sends the new server config.
   *
   * @param parsed - the parsed form JSON
   */
  @bind
  protected submitAdd(parsed: Record<string, unknown>): Promise<unknown> {
    return wsRequest("add_mcp_server", mcpServerPayload(parsed));
  }

  /**
   * Renders one server row.
   *
   * @param info - the server
   */
  @bind
  protected renderServer(info: TReadonly<MCPServerInfo>) {
    return <ServerRow key={info.name} info={info} onRemove={this.removeServer} />;
  }

  /** Renders the dialog. */
  public render() {
    const { onClose } = this.props;
    const { showAdd } = this.state;
    const servers = this.serversView();
    const sorted = [...servers].sort(byName);

    return (
      <div
        className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
        onClick={onClose}
      >
        <div
          className="bg-canvas border border-surface rounded-2xl shadow-xl w-full max-w-lg overflow-hidden flex flex-col max-h-[80vh] chat-font"
          onClick={this.stopPropagation}
        >
          {/* Header */}
          <div className="flex items-center justify-between px-5 py-4 border-b border-surface shrink-0">
            <div>
              <h2 className="text-base font-semibold text-text">MCP Servers</h2>
              <p className="text-xs text-text-subtle mt-0.5">
                {servers.length === 0
                  ? "No servers configured"
                  : `${servers.filter(isEnabled).length} of ${servers.length} enabled`}
              </p>
            </div>
            <button
              onClick={onClose}
              className="text-text-subtle hover:text-text transition-colors p-1 rounded-lg hover:bg-base-overlay"
            >
              <X size={16} />
            </button>
          </div>

          {/* Server list */}
          <div className="flex-1 overflow-y-auto">
            {sorted.length === 0 ? (
              <div className="flex flex-col items-center justify-center py-12 px-6 text-center">
                <p className="text-text-muted text-sm font-medium mb-1">No MCP servers</p>
                <p className="text-text-subtle text-xs">Add a server to get started</p>
              </div>
            ) : (
              sorted.map(this.renderServer)
            )}
          </div>

          {/* Add server */}
          {showAdd ? (
            <MCPForm submitLabel="Add Server" onSubmit={this.submitAdd} onCancel={this.closeAdd} />
          ) : (
            <div className="border-t border-surface px-4 py-3 shrink-0">
              <button
                onClick={this.openAdd}
                className="flex items-center gap-2 w-full px-4 py-2.5 text-sm font-medium text-accent border border-accent/30 rounded-xl hover:bg-accent/5 transition-colors"
              >
                <Plus size={15} />
                Add MCP server
              </button>
            </div>
          )}
        </div>
      </div>
    );
  }
}
