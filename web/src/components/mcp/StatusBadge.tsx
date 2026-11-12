import type { TReadonly } from "@react-carburetor/core";
import { AlertCircle, CheckCircle2, Loader2 } from "lucide-react";
import type { MCPServerInfo } from "../../types";

/**
 * A server's status pill.
 *
 * @param props - component props
 * @param props.info - the server as the backend reports it
 */
export function StatusBadge({ info }: { info: TReadonly<MCPServerInfo> }) {
  if (info.disabled) {
    return (
      <span className="inline-flex items-center gap-1 text-[11px] font-medium text-text-muted bg-base-overlay border border-surface rounded-full px-2 py-0.5">
        Off
      </span>
    );
  }
  switch (info.status) {
    case "connected":
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-green bg-green/10 border border-green/20 rounded-full px-2 py-0.5">
          <CheckCircle2 size={10} />
          Connected · {info.toolCount} tool{info.toolCount !== 1 ? "s" : ""}
        </span>
      );
    case "starting":
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-accent bg-accent/10 border border-accent/20 rounded-full px-2 py-0.5 animate-pulse">
          <Loader2 size={10} className="animate-spin" />
          Starting…
        </span>
      );
    case "error":
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-red bg-red/10 border border-red/20 rounded-full px-2 py-0.5">
          <AlertCircle size={10} />
          Error
        </span>
      );
    default:
      return (
        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-text-subtle bg-base-overlay border border-surface rounded-full px-2 py-0.5">
          {info.status}
        </span>
      );
  }
}
