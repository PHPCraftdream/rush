import { applyTheme } from "./applyTheme";
import { THEME_STORAGE_KEY } from "./THEME_STORAGE_KEY";

// Side effect on import: apply the cached theme immediately on module load
// (before WS connects) so the page does not flash white while waiting for the backend.
try {
  const cached = localStorage.getItem(THEME_STORAGE_KEY);
  if (cached) applyTheme(cached);
} catch {
  /* storage unavailable */
}
