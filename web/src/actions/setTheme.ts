import { ws } from "../net/client";
import { applyTheme } from "./applyTheme";

/**
 * Applies a theme locally and sends it to the backend, the source of truth.
 *
 * @param theme - the chosen theme
 */
export function setTheme(theme: "light" | "dark"): void {
  applyTheme(theme);
  ws.send("set_theme", { theme });
}
