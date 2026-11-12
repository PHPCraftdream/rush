/**
 * The wire fields of an add/update MCP server request, taken from the parsed form JSON.
 *
 * @param parsed - the parsed JSON the operator entered
 */
export function mcpServerPayload(parsed: Record<string, unknown>) {
  return {
    name: (parsed.name as string).trim(),
    type: (parsed.type as string) || "stdio",
    command: parsed.command as string | undefined,
    args: parsed.args as string[] | undefined,
    url: parsed.url as string | undefined,
    env: parsed.env as Record<string, string> | undefined,
    headers: parsed.headers as Record<string, string> | undefined,
    timeout: parsed.timeout as number | undefined,
  };
}
