// Extracts the delegation prompt of an agent tool call.

/**
 * Reads the `prompt` field of an agent tool call's JSON input, falling back to
 * the raw input when it is not JSON or carries no prompt.
 *
 * @param input - the raw tool input
 */
export function agentPrompt(input: string): string {
  try { return JSON.parse(input).prompt ?? input; } catch { return input; }
}
