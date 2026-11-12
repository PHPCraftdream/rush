import type { CustomRole, RoleToolsMode } from "../customRoleTypes";

/**
 * How a role treats tools: inherit the defaults, none, or its own patterns.
 *
 * @param role - the role
 */
export function roleToolsMode(role: CustomRole): RoleToolsMode {
  if (role.tools === undefined) return "inherit";
  return role.tools.length === 0 ? "none" : "patterns";
}
