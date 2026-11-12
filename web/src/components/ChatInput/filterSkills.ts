import type { SkillInfo } from "../../types";

/**
 * Narrows the skills to those whose name or description contains the slash query.
 *
 * @param skills - every skill on offer
 * @param query - text after the slash, or null when the draft is not a bare slash command
 */
export function filterSkills(skills: readonly SkillInfo[], query: string | null): SkillInfo[] {
  if (query === null) return [];
  const q = query.toLowerCase();
  const out: SkillInfo[] = [];
  for (const s of skills) {
    if (s.name.toLowerCase().includes(q) || s.description.toLowerCase().includes(q)) out.push(s);
  }
  return out;
}
