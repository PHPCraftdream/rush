// Live-elapsed / final duration badge for assistant messages.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { Message as Msg } from "../../types";
import { busyCarburetor } from "../../state/busy";
import { isTerminallyFinished } from "./isTerminallyFinished";

/** Props of the duration badge. */
interface IDurationBadgeProps {
  message: TReadonly<Msg>;
}

/** Local state of the duration badge. */
interface IDurationBadgeState {
  elapsed: number;
}

/**
 * Formats a duration in seconds as "12.3s" or "4m 5s".
 *
 * @param s - the duration in seconds
 */
function formatDuration(s: number): string {
  if (s < 60) return `${s.toFixed(1)}s`;
  return `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`;
}

/** Elapsed time of a live turn (ticking every 100 ms), or the final duration once finished. */
export class DurationBadge extends AntiHookComponent<IDurationBadgeProps, IDurationBadgeState> {
  public state: IDurationBadgeState = { elapsed: 0 };

  /**
   * Whether the message is unfinished and its session is busy, read untracked for the effect.
   * Render subscribes to the same flag, so a change re-renders and re-runs the effect pass.
   */
  private liveNow(): boolean {
    const { message } = this.props;
    return !isTerminallyFinished(message.Parts) && busyCarburetor.isBusy(message.SessionID);
  }

  /** Re-declares the ticker whenever liveness or the start time changes. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.runTicker, [this.liveNow(), this.props.message.CreatedAt]);
  }

  /** Starts the 100 ms ticker while live; returns its cleanup. */
  @bind
  protected runTicker(): (() => void) | undefined {
    if (!this.liveNow()) return undefined;
    this.tick();
    const id = setInterval(this.tick, 100);
    return () => clearInterval(id);
  }

  /** Records the seconds elapsed since the message was created. */
  @bind
  protected tick(): void {
    this.setState({ elapsed: Date.now() / 1000 - this.props.message.CreatedAt });
  }

  /** Renders the duration, or nothing under half a second. */
  public render() {
    const { message } = this.props;
    const { busySessions } = this.useCarburetor(busyCarburetor);
    const isFinished = isTerminallyFinished(message.Parts);
    const isLive = !isFinished && busySessions.includes(message.SessionID);
    const duration = isFinished || !isLive ? message.UpdatedAt - message.CreatedAt : this.state.elapsed;
    if (duration < 0.5) return null;
    return <span className="text-xs text-text-subtle font-mono tabular-nums">{formatDuration(duration)}</span>;
  }
}
