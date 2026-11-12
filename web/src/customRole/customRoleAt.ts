import type { CustomRole } from "../customRoleTypes";

/**
 * Looks a role up by own key, so prototype names ("constructor", "__proto__") are never roles.
 *
 * @param roles - roles of one scope
 * @param name - role name
 */
export function customRoleAt(roles: Record<string, CustomRole> | undefined, name: string): CustomRole | undefined {
  return roles && Object.prototype.hasOwnProperty.call(roles, name) ? roles[name] : undefined;
}
