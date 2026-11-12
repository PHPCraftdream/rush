/** Builds a message in wire format: Parts use {type, Text/Thinking/...} with PascalCase. */
export function makeMessage(overrides: Record<string, unknown> = {}) {
  return {
    ID: "msg-1",
    SessionID: "sess-1",
    Role: "user" as const,
    Parts: [{ type: "text", Text: "Hello" }],
    Model: "",
    Provider: "",
    CreatedAt: 1700000000000,
    UpdatedAt: 1700000000000,
    IsSummaryMessage: false,
    ...overrides,
  };
}
