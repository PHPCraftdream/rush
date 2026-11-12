import { AntiHookComponent, bind } from "@react-carburetor/core";
import { ws, wsRequest } from "../../ws";
import { connectionCarburetor } from "../../state/connection";
import { ConfirmDialog } from "../ConfirmDialog";

/** Props of the shutdown confirmation. */
interface IShutdownConfirmProps {
  onClose: () => void;
}

/** Local state of the shutdown confirmation. */
interface IShutdownConfirmState {
  busy: boolean;
  error: string | null;
}

/**
 * Server shutdown confirm (task #714): rare destructive action, wired
 * via wsRequest (task #732) — busy state, inline error, and a bounded
 * timeout/disconnect handler so a drop AFTER the frame was written can no
 * longer leave the dialog busy forever.
 */
export class ShutdownConfirm extends AntiHookComponent<IShutdownConfirmProps, IShutdownConfirmState> {
  public state: IShutdownConfirmState = { busy: false, error: null };

  /**
   * Continuation of confirm() checks this flag after its await so a reply
   * landing after this component unmounted cannot setState on it (the
   * wsRequest listeners themselves are always detached on settle: reply,
   * disconnect, timeout or a failed send).
   */
  private disposed = false;

  /** Declares the mount flag. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.markMounted, []);
  }

  /** Clears the disposed flag on mount; the cleanup sets it on unmount. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Flags the dialog as gone. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  /** Asks the server to shut down; on the ack enters the terminal UI state. */
  @bind
  protected async confirm(): Promise<void> {
    this.setState({ error: null, busy: true });
    try {
      await wsRequest("shutdown_server");
      if (this.disposed) return;
      this.setState({ busy: false });
      // Acked: the server is now shutting down for good. Enter the terminal
      // UI state and stop the auto-reconnect loop from retrying a server
      // that is intentionally gone.
      connectionCarburetor.markServerShuttingDown();
      ws.disableReconnect();
      this.props.onClose();
    } catch (e) {
      if (this.disposed) return;
      this.setState({ busy: false, error: e instanceof Error ? e.message : String(e) });
    }
  }

  /** Renders the dialog. */
  public render() {
    const { busy, error } = this.state;
    return (
      <ConfirmDialog
        title="Shut down server"
        message={
          "This stops the whole server process and cancels any in-progress agent turns. " +
          "The web UI will stop working until the server is started again."
        }
        confirmLabel={busy ? "Shutting down…" : "Shut down"}
        onConfirm={this.confirm}
        onCancel={this.props.onClose}
        error={error}
        busy={busy}
      />
    );
  }
}
