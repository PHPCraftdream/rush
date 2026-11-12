import { AntiHookComponent } from "@react-carburetor/core";
import { connectWS } from "../useWS";

/** Renders nothing; owns the socket for as long as it is mounted. */
export class WsConnector extends AntiHookComponent {
  /** Connects once after the first commit; the teardown disconnects. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(connectWS, []);
  }

  /** Nothing to draw. */
  public render() {
    return null;
  }
}
