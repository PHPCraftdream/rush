import { AntiHookComponent } from "@react-carburetor/core";
import { authCarburetor } from "./state/auth";
import { connectionCarburetor } from "./state/connection";
import { WsConnector } from "./components/WsConnector";
import { Sidebar } from "./components/Sidebar";
import { Chat } from "./components/Chat";
import { Login } from "./components/Login";
import { PowerOff } from "lucide-react";

/** Main shell shown after login: socket wiring, banners, sidebar and chat. */
class AuthedApp extends AntiHookComponent {
  /** Renders the shell; WsConnector goes last so it mounts after its siblings. */
  public render() {
    const { connected, serverShuttingDown: shuttingDown } = this.useCarburetor(connectionCarburetor);

    return (
      <div className="flex h-full overflow-hidden bg-canvas">
        {shuttingDown && (
          // Terminal state after a confirmed shutdown_server request (task #714):
          // distinct from the transient "Reconnecting…" banner — this server is
          // intentionally gone and the client no longer retries.
          <div
            data-test-id="server-shutting-down"
            className="fixed inset-0 z-[100] flex flex-col items-center justify-center gap-4 bg-canvas"
          >
            <div className="w-14 h-14 rounded-2xl bg-red/10 text-red flex items-center justify-center">
              <PowerOff size={26} />
            </div>
            <h2 className="text-lg font-semibold text-text">Server is shutting down</h2>
            <p className="text-sm text-text-subtle max-w-md text-center leading-relaxed">
              The server process was stopped from this page. It is safe to close this tab —
              start the server again to continue working.
            </p>
          </div>
        )}
        {!connected && !shuttingDown && (
          <div className="fixed top-0 inset-x-0 bg-yellow text-base-subtle text-center py-1 text-sm font-semibold z-50">
            Reconnecting…
          </div>
        )}
        <Sidebar />
        <div className="flex-1 flex flex-col overflow-hidden">
          <Chat />
        </div>
        <WsConnector />
      </div>
    );
  }
}

/**
 * Marks the session authenticated when the check response is OK.
 *
 * @param r - the /auth/check response
 */
function applyAuthCheck(r: Response): void {
  if (r.ok) authCarburetor.setAuthed(true);
}

/** Asks the server whether the cookie session is already valid. */
function checkAuth(): void {
  fetch("/auth/check").then(applyAuthCheck);
}

/** Root component: login screen until the session is authenticated. */
export class App extends AntiHookComponent {
  /** Runs the one-time session check after the first commit. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(checkAuth, []);
  }

  /** Renders the login screen or the authenticated app. */
  public render() {
    const { authed } = this.useCarburetor(authCarburetor);
    if (!authed) return <Login />;
    return <AuthedApp />;
  }
}
