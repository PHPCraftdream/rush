/**
 * Extracts the slash-command query: the draft matches "/<word>" with no spaces.
 *
 * @param text - the composer draft
 */
export function slashQueryOf(text: string): string | null {
  const match = text.match(/^\/(\S*)$/);
  return match ? match[1] : null;
}
