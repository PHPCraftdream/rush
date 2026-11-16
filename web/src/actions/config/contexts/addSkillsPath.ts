import { ws } from "../../../backend/ws/client";

/**
 * Adds a skills path to the config.
 *
 * @param path - directory path
 */
export function addSkillsPath(path: string): void {
  ws.send("add_skills_path", { path });
}
