import { Carburetor } from "@react-carburetor/core";
import type { MCPState } from "../types";

/** MCP server health as last reported by the backend; null before the first report. */
export interface IMcpState {
  mcp: MCPState | null;
}

/**
 * Replaces the MCP snapshot in the draft.
 *
 * @param draft - state to change
 * @param mcp - the snapshot the server sent
 */
const writeMcp = (draft: IMcpState, mcp: MCPState | null): void => {
  draft.mcp = mcp;
};

/** Holds the latest MCP snapshot pushed by the server. */
export class McpCarburetor extends Carburetor<IMcpState> {
  /**
   * Replaces the whole snapshot (the server always sends the full state).
   *
   * @param mcp - the snapshot the server sent
   */
  public setMcp(mcp: MCPState | null): void {
    this.update(writeMcp, mcp);
  }
}
