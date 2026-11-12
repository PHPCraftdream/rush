import { AntiHookComponent } from "@react-carburetor/core";
import { CheckCheck } from "lucide-react";
import { formatTokens } from "./formatTokens";
import { pctColor } from "./pctColor";

/** Props of the token pill. */
interface ITokenPillProps {
  totalTokens: number;
  /** Context-window fill in percent, or null when the window is unknown. */
  contextPct: number | null;
  title: string;
  /** Show the "summarized" badge. */
  summarized: boolean;
}

/** Token counter with the context-window fill and a badge once the session has been compacted. */
export class TokenPill extends AntiHookComponent<ITokenPillProps> {
  /** Renders the pill. */
  public render() {
    const { totalTokens, contextPct, title, summarized } = this.props;
    return (
      <span
        data-test-id="header-token-indicator"
        className={
          "text-sm font-medium text-text-subtle bg-base-overlay border border-surface rounded-xl px-3.5 py-2 " +
          "flex items-center gap-1.5"
        }
        title={title}
      >
        {formatTokens(totalTokens)}
        {contextPct !== null && <span className={`font-semibold ${pctColor(contextPct)}`}>{contextPct}%</span>}
        {summarized && (
          <span data-test-id="header-summarized-badge" title="Session has been summarized">
            <CheckCheck size={13} className="text-accent" />
          </span>
        )}
      </span>
    );
  }
}
