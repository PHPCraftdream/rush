/** Builds a config wire object with an anthropic provider and two models. */
export function makeConfig(overrides: Record<string, unknown> = {}) {
  return {
    models: {
      smart: { Provider: "anthropic", Model: "claude-opus-4" },
      fast: { Provider: "anthropic", Model: "claude-haiku-4" },
    },
    providers: {
      anthropic: {
        enabled: true,
        models: [
          { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
          { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
        ],
      },
    },
    yolo: false,
    ...overrides,
  };
}
