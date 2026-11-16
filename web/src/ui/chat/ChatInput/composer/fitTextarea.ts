/** Tallest the composer grows, in px. */
const MAX_COMPOSER_HEIGHT_PX = 720;

/**
 * Grows the composer textarea to fit its content, up to the cap.
 *
 * @param el - the textarea
 */
export function fitTextarea(el: HTMLTextAreaElement): void {
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, MAX_COMPOSER_HEIGHT_PX) + "px";
}
