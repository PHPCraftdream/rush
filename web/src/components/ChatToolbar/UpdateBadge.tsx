import { AntiHookComponent } from "@react-carburetor/core";
import { ArrowUpCircle } from "lucide-react";
import { noticesCarburetor } from "../../state/notices";

/**
 * Link to the releases page, shown only when the server says a newer release exists — it
 * sends nothing otherwise, so there is no "up to date" state to draw. A link, not a
 * self-updater: the fork is installed by the operator and should not rewrite its own
 * binary underneath a running agent session.
 */
export class UpdateBadge extends AntiHookComponent {
  /** Renders the link, or nothing. */
  public render() {
    const { updateAvailable } = this.useCarburetor(noticesCarburetor);
    if (!updateAvailable) return null;
    return (
      <a
        data-test-id="header-update-available"
        href="https://github.com/PHPCraftdream/rush/releases"
        target="_blank"
        rel="noreferrer"
        title={`rush ${updateAvailable.latest} is available (you have ${updateAvailable.current})`}
        className={
          "flex items-center gap-1.5 text-xs font-medium rounded-lg px-2.5 py-1.5 border transition-colors " +
          "bg-base-overlay border-accent/50 text-accent hover:border-accent"
        }
      >
        <ArrowUpCircle size={13} />
        <span>v{updateAvailable.latest}</span>
      </a>
    );
  }
}
