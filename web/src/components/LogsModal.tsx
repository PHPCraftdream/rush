import type { MouseEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { X, RefreshCw, Download } from "lucide-react";
import { wsRequest } from "../ws";

/** Props of the logs dialog. */
interface ILogsModalProps {
  onClose: () => void;
}

/** Local state of the logs dialog. */
interface ILogsModalState {
  logs: string;
  loading: boolean;
  error: string | null;
}

/** Dialog showing the server's recent log lines, with refresh and download. */
export class LogsModal extends AntiHookComponent<ILogsModalProps, ILogsModalState> {
  public state: ILogsModalState = { logs: "", loading: false, error: null };

  // Set when the modal unmounts: fetchLogs' continuation checks it after
  // its await so a late get_logs reply cannot setState on a dead modal
  // (the wsRequest listeners detach themselves on settle).
  private disposed = false;

  /** Declares the lifecycle flag, the mount-time fetch and the Escape listener. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("lifecycle", this.markMounted, []);
    this.useEffect("autoFetch", this.autoFetch, []);
    this.useEffect("escapeKey", this.bindEscape, [this.props.onClose]);
  }

  /** Clears the disposed flag; returns the cleanup that sets it. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Marks the modal as unmounted. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  /** Fetches the logs once on mount. */
  @bind
  protected autoFetch(): void {
    void this.fetchLogs();
  }

  /** Attaches the Escape handler; returns its detach function. */
  @bind
  protected bindEscape(): () => void {
    document.addEventListener("keydown", this.onKey);
    return this.unbindEscape;
  }

  /** Detaches the Escape handler. */
  @bind
  protected unbindEscape(): void {
    document.removeEventListener("keydown", this.onKey);
  }

  /**
   * Closes the dialog on Escape.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    if (e.key === "Escape") this.props.onClose();
  }

  /** Requests the last 1000 log lines. */
  @bind
  protected async fetchLogs(): Promise<void> {
    this.setState({ loading: true, error: null });
    try {
      const reply = await wsRequest<string>("get_logs", { lines: 1000 });
      if (this.disposed) return;
      this.setState({ loading: false, logs: reply.payload ?? "" });
    } catch (e) {
      if (this.disposed) return;
      this.setState({ loading: false, error: e instanceof Error ? e.message : String(e) });
    }
  }

  /** Click handler of the Refresh button. */
  @bind
  protected onRefresh(): void {
    void this.fetchLogs();
  }

  /** Saves the shown logs as a text file. */
  @bind
  protected handleDownload(): void {
    const blob = new Blob([this.state.logs], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `rush-logs-${Date.now()}.txt`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  }

  /**
   * Keeps clicks inside the panel from reaching the backdrop.
   *
   * @param e - the click event
   */
  @bind
  protected stopPropagation(e: MouseEvent): void {
    e.stopPropagation();
  }

  /** Renders the dialog. */
  public render() {
    const { onClose } = this.props;
    const { logs, loading, error } = this.state;
    return (
      <div
        className="fixed inset-0 z-[10000] flex items-center justify-center bg-black/40 backdrop-blur-sm"
        onClick={onClose}
      >
        <div className="bg-canvas border border-surface rounded-2xl shadow-2xl w-[min(800px,90vw)] max-h-[80vh] flex flex-col" onClick={this.stopPropagation}>
          {/* Header */}
          <div className="flex items-center justify-between p-4 border-b border-surface">
            <div className="flex items-center gap-2">
              <h2 className="text-lg font-semibold text-text">Logs</h2>
              <span className="text-sm text-text-subtle">
                {logs.split("\n").length} lines
              </span>
            </div>
            <div className="flex items-center gap-2">
              <button
                onClick={this.onRefresh}
                disabled={loading}
                className="p-2 hover:bg-surface rounded-lg transition-colors disabled:opacity-50"
                title="Refresh logs"
              >
                <RefreshCw size={18} className={loading ? "animate-spin" : ""} />
              </button>
              <button
                onClick={this.handleDownload}
                disabled={!logs || loading}
                className="p-2 hover:bg-surface rounded-lg transition-colors disabled:opacity-50"
                title="Download logs"
              >
                <Download size={18} />
              </button>
              <button
                onClick={onClose}
                className="p-2 hover:bg-surface rounded-lg transition-colors"
              >
                <X size={18} />
              </button>
            </div>
          </div>

          {/* Content */}
          <div className="flex-1 overflow-hidden flex flex-col">
            {error && (
              <div className="p-4 bg-red/10 border border-red/30 rounded-xl m-4">
                <p className="text-red text-sm">{error}</p>
              </div>
            )}

            {loading && (
              <div className="flex items-center justify-center p-8">
                <RefreshCw size={24} className="animate-spin text-text-subtle" />
              </div>
            )}

            {!loading && !error && (
              <pre className="flex-1 overflow-y-auto p-4 text-xs text-text-subtle font-mono bg-surface/30 whitespace-pre-wrap">
                {logs || "No logs available"}
              </pre>
            )}
          </div>
        </div>
      </div>
    );
  }
}
