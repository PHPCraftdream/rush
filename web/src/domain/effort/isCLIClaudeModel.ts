/**
 * Returns true if the model is a CLI Claude model (supports reasoning_effort).
 *
 * @param provider - provider id
 * @param model - model id
 */
export function isCLIClaudeModel(provider: string, model: string): boolean {
  return provider === "local-cli" && (model.startsWith("cli-claude-") || model.startsWith("cli-npx-claude-"));
}
