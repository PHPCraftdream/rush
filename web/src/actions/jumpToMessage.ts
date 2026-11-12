/**
 * Scrolls the transcript to the given message and flashes it briefly.
 *
 * The Message component renders id={`msg-${ID}`} on its row and the
 * .msg-flash CSS class drives a one-second highlight.
 *
 * @param id - message id
 */
export function jumpToMessage(id: string): void {
  const el = document.getElementById(`msg-${id}`);
  if (!el) return;
  el.scrollIntoView({ behavior: "smooth", block: "center" });
  el.classList.remove("msg-flash"); // restart animation if already there
  // Reflow so the class re-add re-triggers the keyframes.
  void (el as HTMLElement).offsetWidth;
  el.classList.add("msg-flash");
  window.setTimeout(() => el.classList.remove("msg-flash"), 1100);
}
