import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import {
  Trash2,
  ToggleLeft,
  ToggleRight,
  ChevronDown,
  ChevronRight,
  Pencil,
  Wrench,
} from "lucide-react";
import type { MCPServerInfo } from "../../types";
import { ws, wsRequest } from "../../ws";
import { MCPForm } from "./MCPForm";
import { StatusBadge } from "./StatusBadge";
import { mcpServerPayload } from "./mcpServerPayload";

/**
 * Renders one tool chip.
 *
 * @param tool - the tool name
 */
const renderTool = (tool: string) => (
  <span
    key={tool}
    className="text-[11px] font-mono text-text-muted bg-base-overlay border border-surface rounded-md px-2 py-0.5"
  >
    {tool}
  </span>
);

// ── Server row ────────────────────────────────────────────────────────────────

/** Props of one server row. */
interface IServerRowProps {
  info: TReadonly<MCPServerInfo>;
  onRemove: (name: string) => void;
}

/** Local state of one server row. */
interface IServerRowState {
  confirmRemove: boolean;
  expanded: boolean;
  editing: boolean;
}

/** One MCP server: status, tools, enable toggle, edit and remove. */
export class ServerRow extends AntiHookComponent<IServerRowProps, IServerRowState> {
  public state: IServerRowState = { confirmRemove: false, expanded: false, editing: false };

  /** Flips the server's disabled flag. */
  @bind
  protected toggle(): void {
    const { info } = this.props;
    ws.send("set_mcp_disabled", { name: info.name, disabled: !info.disabled });
  }

  /** Expands or collapses the tool list, when there are tools. */
  @bind
  protected toggleExpanded(): void {
    if ((this.props.info.tools?.length ?? 0) > 0) this.setState({ expanded: !this.state.expanded });
  }

  /** Opens the editor. */
  @bind
  protected startEditing(): void {
    this.setState({ editing: true });
  }

  /** Closes the editor. */
  @bind
  protected stopEditing(): void {
    this.setState({ editing: false });
  }

  /** Asks for remove confirmation. */
  @bind
  protected askRemove(): void {
    this.setState({ confirmRemove: true });
  }

  /** Dismisses the remove confirmation. */
  @bind
  protected cancelRemove(): void {
    this.setState({ confirmRemove: false });
  }

  /** Confirms removal. */
  @bind
  protected confirmRemove(): void {
    this.props.onRemove(this.props.info.name);
    this.setState({ confirmRemove: false });
  }

  /**
   * Sends the edited server config.
   *
   * @param parsed - the parsed form JSON
   */
  @bind
  protected submitUpdate(parsed: Record<string, unknown>): Promise<unknown> {
    return wsRequest("update_mcp_server", { oldName: this.props.info.name, ...mcpServerPayload(parsed) });
  }

  /** Renders the remove prompt or the remove button. */
  protected renderRemove() {
    if (this.state.confirmRemove) {
      return (
        <div className="flex items-center gap-1 ml-1">
          <span className="text-xs text-text-subtle">Remove?</span>
          <button
            onClick={this.confirmRemove}
            className="px-2 py-0.5 text-xs font-medium bg-red-fill text-white/90 rounded hover:opacity-90 transition-opacity"
          >Yes</button>
          <button
            onClick={this.cancelRemove}
            className="px-2 py-0.5 text-xs text-text-subtle hover:text-text transition-colors rounded"
          >No</button>
        </div>
      );
    }
    return (
      <button
        onClick={this.askRemove}
        title="Remove server"
        className="p-1 text-text-subtle hover:text-red transition-colors rounded"
      >
        <Trash2 size={14} />
      </button>
    );
  }

  /** Renders the row, or the editor while editing. */
  public render() {
    const { info } = this.props;
    const { expanded, editing } = this.state;

    if (editing) {
      return (
        <div className="border-b border-surface last:border-0">
          <MCPForm
            initial={info}
            submitLabel="Update Server"
            onSubmit={this.submitUpdate}
            onCancel={this.stopEditing}
          />
        </div>
      );
    }

    const hasTools = (info.tools?.length ?? 0) > 0;
    const expandTone = hasTools ? "text-text-subtle hover:text-text cursor-pointer" : "text-surface cursor-default";
    const toggleTone = info.disabled ? "text-text-subtle hover:text-accent" : "text-accent hover:text-accent/70";

    return (
      <div className="border-b border-surface last:border-0">
        <div className="flex items-center gap-3 px-4 py-3">
          {/* Expand tools toggle */}
          <button
            onClick={this.toggleExpanded}
            className={`shrink-0 transition-colors ${expandTone}`}
            title={hasTools ? (expanded ? "Hide tools" : "Show tools") : "No tools"}
          >
            {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          </button>

          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2">
              <span className="text-sm font-semibold text-text truncate">{info.name}</span>
              <StatusBadge info={info} />
              {info.source === "external" && (
                <span className="px-1.5 py-0.5 text-[10px] font-mono bg-surface text-text-subtle rounded">
                  .mcp.json
                </span>
              )}
            </div>
            {info.serverType && (
              <span className="text-[11px] text-text-subtle font-mono">
                {info.serverType}
                {info.command ? ` · ${info.command}` : ""}
                {info.url ? ` · ${info.url}` : ""}
              </span>
            )}
          </div>

          <div className="flex items-center gap-1 shrink-0">
            {/* Edit (not available for external/.mcp.json servers) */}
            {info.source !== "external" && (
              <button
                onClick={this.startEditing}
                title="Edit server"
                className="p-1 text-text-subtle hover:text-accent transition-colors rounded"
              >
                <Pencil size={14} />
              </button>
            )}

            {/* Toggle */}
            <button
              onClick={this.toggle}
              title={info.disabled ? "Enable" : "Disable"}
              className={`transition-colors ${toggleTone}`}
            >
              {info.disabled ? <ToggleLeft size={22} /> : <ToggleRight size={22} />}
            </button>

            {/* Remove (not available for external/.mcp.json servers) */}
            {info.source !== "external" && this.renderRemove()}
          </div>
        </div>

        {/* Tools list */}
        {expanded && hasTools && (
          <div className="px-4 pb-3 pl-10">
            <div className="flex items-center gap-1.5 mb-2">
              <Wrench size={11} className="text-text-subtle" />
              <span className="text-[11px] font-semibold text-text-subtle uppercase tracking-wider">Tools</span>
            </div>
            <div className="flex flex-wrap gap-1.5">
              {info.tools!.map(renderTool)}
            </div>
          </div>
        )}
      </div>
    );
  }
}
