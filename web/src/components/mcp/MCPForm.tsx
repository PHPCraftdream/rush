import { createRef } from "react";
import type { ChangeEvent, KeyboardEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { X, Plus, AlertCircle, Loader2 } from "lucide-react";
import type { MCPServerInfo } from "../../types";
import { PLACEHOLDER_JSON } from "./PLACEHOLDER_JSON";
import { buildInitialJson } from "./buildInitialJson";

// ── Add / Edit MCP form ───────────────────────────────────────────────────────

/** Props of the MCP server form. */
interface IMCPFormProps {
  initial?: TReadonly<MCPServerInfo>;
  submitLabel: string;
  onSubmit: (parsed: Record<string, unknown>) => Promise<unknown>;
  onCancel: () => void;
}

/** Local state of the MCP server form. */
interface IMCPFormState {
  json: string;
  error: string | null;
  busy: boolean;
}

/** JSON editor for adding or editing one MCP server. */
export class MCPForm extends AntiHookComponent<IMCPFormProps, IMCPFormState> {
  public state: IMCPFormState = {
    json: this.props.initial ? buildInitialJson(this.props.initial) : "",
    error: null,
    busy: false,
  };

  private readonly taRef = createRef<HTMLTextAreaElement>();

  // Continuation of submit() checks this flag after its await so a reply
  // landing after this form was cancelled cannot fire onCancel on a later
  // form (the wsRequest listeners themselves are always detached on
  // settle: reply, disconnect, timeout or a failed send).
  private disposed = false;

  /** Focuses the editor once and tracks mount state. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("focusEditor", this.focusEditor, []);
    this.useEffect("lifecycle", this.markMounted, []);
  }

  /** Focuses the textarea. */
  @bind
  protected focusEditor(): void {
    this.taRef.current?.focus();
  }

  /** Clears the disposed flag; returns the cleanup that sets it. */
  @bind
  protected markMounted(): () => void {
    this.disposed = false;
    return this.markDisposed;
  }

  /** Marks the form as unmounted. */
  @bind
  protected markDisposed(): void {
    this.disposed = true;
  }

  /** Validates the JSON and submits it; closes the form on success. */
  @bind
  protected async submit(): Promise<void> {
    const { onSubmit, onCancel } = this.props;
    this.setState({ error: null });
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(this.state.json.trim());
    } catch {
      this.setState({ error: "Invalid JSON — check your syntax" });
      return;
    }
    const name = (parsed.name as string | undefined)?.trim();
    if (!name) {
      this.setState({ error: '"name" field is required' });
      return;
    }
    const type = (parsed.type as string | undefined) || "stdio";
    if (!["stdio", "http", "sse"].includes(type)) {
      this.setState({ error: '"type" must be "stdio", "http", or "sse"' });
      return;
    }

    this.setState({ busy: true });
    try {
      await onSubmit(parsed);
      if (this.disposed) return;
      this.setState({ busy: false });
      onCancel();
    } catch (e) {
      if (this.disposed) return;
      this.setState({ busy: false, error: e instanceof Error ? e.message : String(e) });
    }
  }

  /** Click handler of the submit button. */
  @bind
  protected onSubmitClick(): void {
    void this.submit();
  }

  /**
   * Escape cancels.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent): void {
    if (e.key === "Escape") this.props.onCancel();
  }

  /**
   * Stores the typed JSON.
   *
   * @param e - the textarea change event
   */
  @bind
  protected onJsonChange(e: ChangeEvent<HTMLTextAreaElement>): void {
    this.setState({ json: e.target.value });
  }

  /** Renders the form. */
  public render() {
    const { initial, submitLabel, onCancel } = this.props;
    const { json, error, busy } = this.state;
    return (
      <div className="border-t border-surface bg-base-subtle/50 p-4">
        <div className="flex items-center justify-between mb-3">
          <h3 className="text-sm font-semibold text-text">
            {initial ? `Edit: ${initial.name}` : "Add MCP Server"}
          </h3>
          <button onClick={onCancel} className="text-text-subtle hover:text-text transition-colors">
            <X size={15} />
          </button>
        </div>
        {!initial && (
          <p className="text-xs text-text-subtle mb-3 leading-relaxed">
            Paste the server config as JSON. The server will be connected immediately.
          </p>
        )}
        <textarea
          ref={this.taRef}
          value={json}
          onChange={this.onJsonChange}
          onKeyDown={this.onKey}
          placeholder={initial ? undefined : PLACEHOLDER_JSON}
          rows={8}
          spellCheck={false}
          className="w-full font-mono text-xs text-text bg-canvas border border-surface rounded-xl px-3 py-2.5 resize-none outline-none focus:border-accent/50 transition-colors placeholder:text-text-muted/50"
        />
        {error && (
          <p className="text-xs text-red mt-2 flex items-center gap-1.5">
            <AlertCircle size={12} />
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2 mt-3">
          <button
            onClick={onCancel}
            className="px-3 py-1.5 text-xs text-text-subtle hover:text-text transition-colors rounded-lg hover:bg-base-overlay"
          >
            Cancel
          </button>
          <button
            onClick={this.onSubmitClick}
            disabled={!json.trim() || busy}
            className="px-4 py-1.5 text-xs font-semibold bg-accent-fill text-white/90 rounded-lg hover:opacity-90 transition-opacity disabled:opacity-40 flex items-center gap-1.5"
          >
            {busy ? <Loader2 size={12} className="animate-spin" /> : <Plus size={12} />}
            {busy ? "Connecting…" : submitLabel}
          </button>
        </div>
      </div>
    );
  }
}
