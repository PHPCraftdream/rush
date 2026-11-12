import { AntiHookComponent } from "@react-carburetor/core";
import { ChevronsDownUp } from "lucide-react";
import { signalsCarburetor } from "../../state/signals";

/** Closes every collapsible spoiler in the transcript. */
const collapseAllSpoilers = (): void => {
  signalsCarburetor.collapseAll();
};

/** Folds every tool group, thinking block, summary and background-job notice in the chat. */
export class CollapseAllButton extends AntiHookComponent {
  /** Renders the button. */
  public render() {
    return (
      <button
        data-test-id="header-collapse-all-button"
        onClick={collapseAllSpoilers}
        title={
          "Collapse all spoilers — fold every tool group, thinking block, summary and " +
          "background-job notice in this chat"
        }
        className={
          "flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors " +
          "bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        }
      >
        <ChevronsDownUp size={13} />
        <span>Collapse all</span>
      </button>
    );
  }
}
