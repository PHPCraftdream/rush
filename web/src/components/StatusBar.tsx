import { AntiHookComponent } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { MCPServerInfo } from "../types";
import { connectionCarburetor } from "../state/connection";
import { mcpCarburetor } from "../state/mcp";

/** Props of the status strip. */
interface IStatusBarProps {
  /** Drops the standalone strip styling so the widget fits inside ChatToolbar's row. */
  inline?: boolean;
}

/**
 * Maps an MCP server status to its indicator colour class.
 *
 * @param status - the server's reported status
 */
function mcpDot(status: string): string {
  switch (status) {
    case "connected":
    case "running":
      return "bg-green";
    case "connecting":
      return "bg-yellow";
    case "error":
    case "disconnected":
      return "bg-red";
    default:
      return "bg-surface";
  }
}

/**
 * Renders one MCP server's status dot and name.
 *
 * @param srv - the server entry
 */
const renderMcpServer = (srv: TReadonly<MCPServerInfo>) => (
  <div
    key={srv.name}
    data-test-id={`status-mcp-${srv.name}`}
    className="flex items-center gap-1"
    title={`${srv.name}: ${srv.status}`}
  >
    <span className={`w-1.5 h-1.5 rounded-full inline-block ${mcpDot(srv.status)}`} />
    <span>{srv.name}</span>
  </div>
);

// StatusBar shows the WebSocket connection state and MCP server health.
// `inline` switches off the standalone strip styling (border, background,
// extra padding) so the same widget can sit in the middle of ChatToolbar
// without breaking that row's flex layout. Default behaviour matches the
// original standalone footer strip.
export class StatusBar extends AntiHookComponent<IStatusBarProps> {
  /** Renders the connection dot and, when servers exist, the MCP dots. */
  public render() {
    const { inline = false } = this.props;
    const { mcp: mcpState } = this.useCarburetor(mcpCarburetor);
    const { connected } = this.useCarburetor(connectionCarburetor);

    const outerClass = inline
      ? "flex items-center gap-3 text-xs text-text-subtle shrink-0 min-w-0 overflow-x-auto"
      : "flex items-center gap-4 px-5 py-2 border-t border-surface bg-base-subtle text-xs text-text-subtle shrink-0 overflow-x-auto";

    return (
      <div data-test-id="status-bar" className={outerClass}>
        {/* Connection */}
        <div data-test-id="status-connection" className="flex items-center gap-1.5 shrink-0">
          <span
            className={`w-1.5 h-1.5 rounded-full inline-block ${connected ? "bg-green" : "bg-red"}`}
          />
          <span>{connected ? "Connected" : "Disconnected"}</span>
        </div>

        {/* MCP */}
        {mcpState && (mcpState.servers?.length ?? 0) > 0 && (
          <>
            <span className="text-surface">|</span>
            <div data-test-id="status-mcp" className="flex items-center gap-3 shrink-0">
              <span className="text-text-subtle font-semibold">MCP</span>
              {(mcpState.servers ?? []).map(renderMcpServer)}
            </div>
          </>
        )}
      </div>
    );
  }
}
