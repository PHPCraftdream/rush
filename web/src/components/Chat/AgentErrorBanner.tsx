import { AntiHookComponent } from "@react-carburetor/core";
import { X } from "lucide-react";
import { noticesCarburetor } from "../../state/notices";

/** Props of the agent error banner. */
interface IAgentErrorBannerProps {
  message: string;
}

/** Dismisses the agent error banner. */
const dismissAgentError = (): void => {
  noticesCarburetor.clearAgentError();
};

/** Red banner under the transcript showing the last agent error. */
export class AgentErrorBanner extends AntiHookComponent<IAgentErrorBannerProps> {
  /** Renders the banner with its dismiss button. */
  public render() {
    return (
      <div className="px-5 py-2">
        <div className="chat-error-banner">
          <span className="text-red text-lg shrink-0 mt-0.5">⚠</span>
          <p className="text-[15px] text-red/80 leading-relaxed flex-1 break-words">{this.props.message}</p>
          <button
            onClick={dismissAgentError}
            aria-label="Dismiss"
            className="text-red/40 hover:text-red/70 transition-colors shrink-0 text-xl leading-none mt-0.5"
          >
            <X size={16} />
          </button>
        </div>
      </div>
    );
  }
}
