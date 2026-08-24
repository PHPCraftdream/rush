import { useEffect } from "react";
import { useStore } from "@nanostores/react";
import { $authed, $connected, $serverShuttingDown } from "./store";
import { useWS } from "./useWS";
import { Sidebar } from "./components/Sidebar";
import { Chat } from "./components/Chat";
import { Login } from "./components/Login";
import { PowerOff } from "lucide-react";

function AuthedApp() {
  useWS();
  const connected = useStore($connected);
  const shuttingDown = useStore($serverShuttingDown);

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
            The server process was stopped from this page. It is safe to close this tab — start the server again to continue working.
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
    </div>
  );
}

export function App() {
  const authed = useStore($authed);

  useEffect(() => {
    fetch("/auth/check").then((r) => {
      if (r.ok) $authed.set(true);
    });
  }, []);

  if (!authed) return <Login />;
  return <AuthedApp />;
}
