import type { TReadonly } from "@react-carburetor/core";
import type { MCPServerInfo } from "../../types";

/**
 * The editable JSON of an existing server.
 *
 * @param info - the server as the backend reports it
 */
export function buildInitialJson(info: TReadonly<MCPServerInfo>): string {
  const obj: Record<string, unknown> = { name: info.name };
  if (info.serverType) obj.type = info.serverType;
  if (info.command) obj.command = info.command;
  if (info.args?.length) obj.args = info.args;
  if (info.url) obj.url = info.url;
  if (info.env && Object.keys(info.env).length > 0) obj.env = info.env;
  if (info.headers && Object.keys(info.headers).length > 0) obj.headers = info.headers;
  return JSON.stringify(obj, null, 2);
}
