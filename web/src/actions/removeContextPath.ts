import { ws } from "../net/client";

/**
 * Removes a context path from the config.
 *
 * @param path - directory or file path
 */
export function removeContextPath(path: string): void {
  ws.send("remove_context_path", { path });
}
