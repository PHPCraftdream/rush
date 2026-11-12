/** Builds a session wire object with sensible defaults. */
export function makeSession(overrides: Record<string, unknown> = {}) {
  return {
    ID: "sess-1",
    Title: "Test Session",
    MessageCount: 0,
    PromptTokens: 0,
    CompletionTokens: 0,
    Cost: 0,
    Todos: [],
    CreatedAt: 1700000000000,
    UpdatedAt: 1700000000000,
    ParentSessionID: "",
    YoloEnabled: false,
    ...overrides,
  };
}
