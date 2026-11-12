import type { ToolResult } from "../types";

/** The job fields of a tool_result's metadata JSON. */
export interface IResultMetadata {
  async?: boolean;
  job_id?: string;
  background?: boolean;
  shell_id?: string;
}

/**
 * Parses a tool_result's metadata, or null when absent or malformed.
 *
 * @param part - the tool result
 */
export function resultMetadata(part: ToolResult): IResultMetadata | null {
  if (!part.Metadata) return null;
  try {
    return JSON.parse(part.Metadata);
  } catch {
    return null;
  }
}
