import { ws } from "../net/client";

/**
 * Removes a skills path from the config.
 *
 * @param path - directory path
 */
export function removeSkillsPath(path: string): void {
  ws.send("remove_skills_path", { path });
}
