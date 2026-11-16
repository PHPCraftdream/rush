/** Source badge colors. */
const SOURCE_COLORS: Record<string, string> = {
  claude: "bg-[#d97706]/15 text-[#d97706]",
  gemini: "bg-[#1a73e8]/15 text-[#1a73e8]",
  qwen: "bg-[#7c3aed]/15 text-[#7c3aed]",
  cursor: "bg-[#0ea5e9]/15 text-[#0ea5e9]",
  zed: "bg-[#059669]/15 text-[#059669]",
  windsurf: "bg-[#0891b2]/15 text-[#0891b2]",
  rush: "bg-accent/15 text-accent",
  local: "bg-surface text-text-muted",
};

/**
 * Picks the badge colour classes for a skill source.
 *
 * @param source - the skill's source, if any
 */
export function sourceBadgeClass(source?: string): string {
  return SOURCE_COLORS[source ?? ""] ?? SOURCE_COLORS.local;
}
