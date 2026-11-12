import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Square } from "lucide-react";
import { ws } from "../../ws";

/** Props of the busy indicator. */
interface IBusyIndicatorProps {
  sessionID: string | null;
}

/** Pulsing dots with a Stop button while the active session's agent runs. */
export class BusyIndicator extends AntiHookComponent<IBusyIndicatorProps> {
  /** Asks the server to cancel the running turn. */
  @bind
  protected stop(): void {
    const { sessionID } = this.props;
    if (sessionID) ws.send("cancel_agent", { sessionID });
  }

  /** Renders the dots and the Stop button. */
  public render() {
    return (
      <div className="flex items-center gap-3 px-5 py-2">
        <div className="flex gap-1.5 animate-pulse-dots">
          <span className="w-2 h-2 rounded-full bg-accent inline-block" />
          <span className="w-2 h-2 rounded-full bg-accent inline-block" />
          <span className="w-2 h-2 rounded-full bg-accent inline-block" />
        </div>
        <button onClick={this.stop} className="btn-stop">
          <Square size={11} fill="currentColor" />
          Stop
        </button>
      </div>
    );
  }
}
