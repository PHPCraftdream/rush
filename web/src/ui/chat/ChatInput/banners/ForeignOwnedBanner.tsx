import { AntiHookComponent } from "@react-carburetor/core";
import { Zap } from "lucide-react";

/** Props of the read-only banner. */
interface IForeignOwnedBannerProps {
  ownerPID: number;
}

/**
 * Foreign-owned: another live rush process holds the session lock.
 * We can read the conversation but can't drive it — the input row and
 * all action buttons are replaced by this read-only banner.
 */
export class ForeignOwnedBanner extends AntiHookComponent<IForeignOwnedBannerProps> {
  /** Renders the banner. */
  public render() {
    const { ownerPID } = this.props;
    return (
      <div className="px-5 pt-2 pb-4 bg-canvas shrink-0">
        <div
          data-test-id="chat-input-foreign-owned-banner"
          className="rounded-xl border border-yellow/40 bg-yellow/10 text-yellow px-4 py-3 text-sm flex items-center gap-2"
          title={
            "Read-only follow mode. Another live rush process holds the session lock — " +
            "this tab polls the database for updates instead of driving the agent."
          }
        >
          <Zap size={14} className="shrink-0" />
          <span>
            Read-only follow mode — session is driven by another rush process
            {ownerPID > 0 ? ` (PID ${ownerPID})` : ""}.
          </span>
        </div>
      </div>
    );
  }
}
