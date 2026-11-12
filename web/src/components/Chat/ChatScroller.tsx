import { createRef } from "react";
import type { ReactNode, WheelEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { Message as Msg } from "../../types";
import { clearSelection } from "./clearSelection";

/** Props of the scrolling transcript container. */
interface IChatScrollerProps {
  sessionID: string | null;
  messages: TReadonly<Msg[]>;
  isBusy: boolean;
  agentError: string | null;
  children: ReactNode;
}

/** Scroll-magnet engages while the view sits within this many px of the bottom. */
const BOTTOM_SLACK_PX = 80;

/**
 * The scrolling transcript pane.
 *
 * Owns the auto-scroll "magnet": it follows new content while the view is at the
 * bottom, and scrolling up disengages it. Switching session resets the selection
 * and snaps back to the bottom.
 */
export class ChatScroller extends AntiHookComponent<IChatScrollerProps> {
  private readonly bottomRef = createRef<HTMLDivElement>();

  private readonly scrollRef = createRef<HTMLDivElement>();

  private isAtBottom = true;

  /** Declares the session-switch reset and the content-follow effects. */
  protected useEffects(): void {
    super.useEffects();
    const { sessionID, messages, isBusy, agentError } = this.props;
    this.useEffect(this.resetOnSessionChange, [sessionID]);
    this.useEffect(this.followContent, [messages, isBusy, agentError]);
  }

  /** New session: drop the batch selection, re-engage the magnet and jump to the bottom. */
  @bind
  protected resetOnSessionChange(): void {
    clearSelection();
    this.isAtBottom = true;
    this.bottomRef.current?.scrollIntoView({ behavior: "instant" });
  }

  /** New content: stay glued to the bottom while the magnet is engaged. */
  @bind
  protected followContent(): void {
    if (this.isAtBottom) {
      this.bottomRef.current?.scrollIntoView({ behavior: "instant" });
    }
  }

  /** Re-evaluates the magnet from the scroll position. */
  @bind
  protected handleScroll(): void {
    const el = this.scrollRef.current;
    if (!el) return;
    this.isAtBottom = el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_SLACK_PX;
  }

  /**
   * Disengages the magnet on scroll-up; Shift+wheel scrolls 5x faster.
   *
   * Scroll-up must disengage the auto-scroll magnet synchronously, right
   * here -- waiting on handleScroll's position check races against the
   * content effect, which re-fires on every streamed token and can
   * snap back to bottom before the position check catches up. Re-engages
   * normally once handleScroll sees the user back within 80px of bottom.
   *
   * @param e - the wheel event
   */
  @bind
  protected handleWheel(e: WheelEvent<HTMLDivElement>): void {
    if (e.deltaY < 0) this.isAtBottom = false;
    if (e.shiftKey) {
      const el = this.scrollRef.current;
      if (!el) return;
      e.preventDefault();
      // Scroll 5x faster when Shift is held
      el.scrollTop += e.deltaY * 5;
    }
  }

  /** Renders the container, its content and the bottom anchor. */
  public render() {
    return (
      <div
        ref={this.scrollRef}
        onScroll={this.handleScroll}
        onWheel={this.handleWheel}
        data-test-id="chat-scroll-container"
        className="flex-1 overflow-y-auto overflow-x-hidden py-8 flex flex-col"
      >
        {this.props.children}
        <div ref={this.bottomRef} className="h-8 shrink-0" />
      </div>
    );
  }
}
