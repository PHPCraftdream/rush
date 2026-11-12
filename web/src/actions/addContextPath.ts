import { ws } from "../net/client";

/**
 * Adds a context path to the config.
 *
 * @param path - directory or file path
 */
export function addContextPath(path: string): void {
  ws.send("add_context_path", { path });
}
