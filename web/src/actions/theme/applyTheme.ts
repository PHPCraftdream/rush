import { THEME_STORAGE_KEY } from "./THEME_STORAGE_KEY";

/**
 * Applies a theme to the document and caches it for the next page load.
 *
 * Theme is stored on the backend; localStorage is only a cache for instant page
 * load (before WS connects) to avoid a white flash.
 *
 * @param theme - "dark" or anything else for light
 */
export function applyTheme(theme: string): void {
  if (theme === "dark") {
    document.documentElement.classList.add("dark");
  } else {
    document.documentElement.classList.remove("dark");
  }
  // Cache to localStorage for instant load on next page refresh
  try {
    localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    /* storage unavailable */
  }
}
