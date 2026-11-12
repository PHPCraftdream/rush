import { SkillsCarburetor } from "./SkillsCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const skillsCarburetor = new SkillsCarburetor({ skills: [], lastUsedSkill: SkillsCarburetor.readStoredSkill() });
