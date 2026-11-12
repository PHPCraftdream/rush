// Custom-role wire types plus the role helpers (one export per file in ./customRole).
export { customRoleAt } from "./customRole/customRoleAt";
export { roleToolsMode } from "./customRole/roleToolsMode";
export { roleToolsPayload } from "./customRole/roleToolsPayload";

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
