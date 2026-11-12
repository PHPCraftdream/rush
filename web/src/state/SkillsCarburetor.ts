import { Carburetor } from "@react-carburetor/core";
import type { SkillInfo } from "../types";

/** The skills the server offers and the one the composer used last. */
export interface ISkillsState {
  skills: SkillInfo[];
  lastUsedSkill: string;
}

/**
 * Replaces the skills list in the draft.
 *
 * @param draft - state to change
 * @param skills - the new list
 */
const writeSkills = (draft: ISkillsState, skills: SkillInfo[]): void => {
  draft.skills = skills;
};

/**
 * Writes the last used skill name into the draft.
 *
 * @param draft - state to change
 * @param name - skill name
 */
const writeLastUsed = (draft: ISkillsState, name: string): void => {
  draft.lastUsedSkill = name;
};

/** Holds the skills list; the last used skill is mirrored to localStorage. */
export class SkillsCarburetor extends Carburetor<ISkillsState> {
  /** localStorage key of the last used skill. */
  public static readonly LAST_SKILL_KEY = "rush_last_skill";

  /** The remembered last used skill, or "" when none was stored. */
  public static readStoredSkill(): string {
    return localStorage.getItem(SkillsCarburetor.LAST_SKILL_KEY) ?? "";
  }

  /**
   * Replaces the skills list.
   *
   * @param skills - the list the server sent
   */
  public setSkills(skills: SkillInfo[]): void {
    this.update(writeSkills, skills);
  }

  /**
   * Remembers the skill the composer used and persists it.
   *
   * @param name - skill name
   */
  public setLastUsedSkill(name: string): void {
    this.update(writeLastUsed, name);
    localStorage.setItem(SkillsCarburetor.LAST_SKILL_KEY, name);
  }
}
