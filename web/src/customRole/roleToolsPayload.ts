import type { RoleToolsMode, SetCustomRoleRequest } from "../customRoleTypes";

/**
 * Whether a value is a non-empty string.
 *
 * @param pattern - the value
 */
const isPattern = (pattern: unknown): boolean => typeof pattern === "string" && pattern.length > 0;

/**
 * Builds the tools part of a set-role request from the editor's mode and pattern text.
 *
 * @param mode - the selected tools mode
 * @param text - JSON array of tool patterns (used by the patterns mode)
 */
export function roleToolsPayload(mode: RoleToolsMode, text: string): Pick<SetCustomRoleRequest, "tools" | "inherit_tools"> {
  if (mode === "inherit") return { inherit_tools: true };
  if (mode === "none") return { tools: [] };
  const patterns: unknown = JSON.parse(text);
  if (!Array.isArray(patterns) || !patterns.every(isPattern)) {
    throw new Error("Tool patterns must be a JSON array of non-empty strings.");
  }
  return { tools: patterns };
}
