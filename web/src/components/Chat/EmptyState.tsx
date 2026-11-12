import { AntiHookComponent } from "@react-carburetor/core";
import { MessageSquare, Sparkles } from "lucide-react";

/** Props of the transcript placeholder. */
interface IEmptyStateProps {
  /** True when no session is selected; false when the selected one has no messages. */
  noSession: boolean;
}

/** Placeholder shown instead of the transcript when there is nothing to list. */
export class EmptyState extends AntiHookComponent<IEmptyStateProps> {
  /** Renders the "no session" or "no messages" card. */
  public render() {
    if (this.props.noSession) {
      return (
        <div className="empty-state">
          <div className="empty-state-icon">
            <MessageSquare size={40} />
          </div>
          <p className="empty-state-title">No session selected</p>
          <p className="empty-state-desc">Select a session from the sidebar or create a new one</p>
        </div>
      );
    }
    return (
      <div className="empty-state">
        <div className="empty-state-icon">
          <Sparkles size={40} />
        </div>
        <p className="empty-state-title">No messages yet</p>
        <p className="empty-state-desc">Say something to get started</p>
      </div>
    );
  }
}
