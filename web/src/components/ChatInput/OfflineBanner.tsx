import { AntiHookComponent } from "@react-carburetor/core";
import { WifiOff } from "lucide-react";

/** Props of the offline banner. */
interface IOfflineBannerProps {
  outboxCount: number;
}

/** Notice that the socket is down and sent messages are held for delivery on reconnect. */
export class OfflineBanner extends AntiHookComponent<IOfflineBannerProps> {
  /** Renders the notice with the parked-frame count. */
  public render() {
    const { outboxCount } = this.props;
    return (
      <div
        data-test-id="chat-input-offline-indicator"
        className="mb-2 rounded-xl border border-yellow/40 bg-yellow/10 text-yellow px-4 py-2.5 text-xs flex items-center gap-2"
        title={
          "The WebSocket connection to the server is down. Messages you send from here are held locally " +
          "and delivered automatically when the connection returns."
        }
      >
        <WifiOff size={13} className="shrink-0" />
        <span>
          Offline — messages you send are held locally
          {outboxCount > 0 ? ` (${outboxCount} queued)` : ""} and go out when the connection returns.
        </span>
      </div>
    );
  }
}
