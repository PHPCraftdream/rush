/** Builds a permission request wire object. */
export function makePermission(overrides: Record<string, unknown> = {}) {
  return {
    ID: "perm-1",
    SessionID: "sess-1",
    ToolCallID: "tc-1",
    ToolName: "bash",
    Description: "Run a shell command",
    Action: "execute",
    Path: "/tmp/script.sh",
    Params: {},
    ...overrides,
  };
}
