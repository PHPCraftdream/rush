export interface CustomRole {
  provider: string;
  model: string;
  reasoning_effort?: string;
  think?: boolean;
  tools?: string[];
}

export interface CustomRolesState {
  global: Record<string, CustomRole>;
  workspace: Record<string, CustomRole>;
  effective: Record<string, CustomRole>;
  hasWorkspace: boolean;
}

export type CustomRoleScope = "global" | "workspace";
export type RoleToolsMode = "inherit" | "none" | "patterns";

export function customRoleAt(roles: Record<string, CustomRole> | undefined, name: string): CustomRole | undefined {
  return roles && Object.prototype.hasOwnProperty.call(roles, name) ? roles[name] : undefined;
}

export interface SetCustomRoleRequest {
  scope: CustomRoleScope;
  name: string;
  provider: string;
  model: string;
  reasoning_effort?: string;
  think?: boolean;
  tools?: string[];
  inherit_tools?: boolean;
}

export function roleToolsMode(role: CustomRole): RoleToolsMode {
  if (role.tools === undefined) return "inherit";
  return role.tools.length === 0 ? "none" : "patterns";
}

export function roleToolsPayload(mode: RoleToolsMode, text: string): Pick<SetCustomRoleRequest, "tools" | "inherit_tools"> {
  if (mode === "inherit") return { inherit_tools: true };
  if (mode === "none") return { tools: [] };
  const patterns: unknown = JSON.parse(text);
  if (!Array.isArray(patterns) || !patterns.every((pattern) => typeof pattern === "string" && pattern.length > 0)) {
    throw new Error("Tool patterns must be a JSON array of non-empty strings.");
  }
  return { tools: patterns };
}
