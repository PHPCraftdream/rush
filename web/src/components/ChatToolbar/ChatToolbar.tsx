import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { busyCarburetor } from "../../state/busy";
import { sessionsCarburetor } from "../../state/sessions";
import type { ISessionsState } from "../../state/SessionsCarburetor";
import { findSession } from "../Chat/findSession";
import { StatusBar } from "../StatusBar";
import { BusyDots } from "./BusyDots";
import { CollapseAllButton } from "./CollapseAllButton";
import { CompactButton } from "./CompactButton";
import { KeepAliveToggle } from "./KeepAliveToggle";
import { ModelCluster } from "./ModelCluster";
import { MoreMenu } from "./MoreMenu";
import { SitterPill } from "./SitterPill";
import { ThemeToggle } from "./ThemeToggle";
import { TokenPill } from "./TokenPill";
import { ToolbarModals } from "./ToolbarModals";
import type { ToolbarModal } from "./ToolbarModal";
import { UpdateBadge } from "./UpdateBadge";
import { activeSmartModel } from "./activeSmartModel";

/** Local state of the toolbar. */
interface IChatToolbarState {
  modal: ToolbarModal | null;
  showRoleModels: boolean;
  moreOpen: boolean;
}

/**
 * Picks the active session out of the sessions state.
 *
 * @param state - the sessions state
 */
const selectActiveSession = (state: TReadonly<ISessionsState>) => findSession(state.sessions, state.activeSessionID);

/**
 * Flips the More menu.
 *
 * @param s - the toolbar state
 */
const flipMoreOpen = (s: IChatToolbarState): Pick<IChatToolbarState, "moreOpen"> => ({ moreOpen: !s.moreOpen });

/**
 * Flips the worker/reviewer selectors.
 *
 * @param s - the toolbar state
 */
const flipRoleModels = (s: IChatToolbarState): Pick<IChatToolbarState, "showRoleModels"> => ({
  showRoleModels: !s.showRoleModels,
});

/**
 * The toolbar above the composer: token meter, compact, menus, toggles and model selectors.
 *
 * No `if (!activeSessionID) return null;` here — deliberately. This toolbar absorbed the old
 * standalone Header.tsx in commit 89a07919, whose message claimed "No behaviour changes other
 * than where each control physically renders", yet it also added exactly that early return,
 * which Header never had. With it, the theme toggle, the settings / MCP / providers / logs /
 * prompt buttons and the model badges were all unreachable until a session was selected. We
 * render with no session again; only genuinely session-bound controls are hidden individually
 * (Compact; Prompt inside the "More" dropdown; the token pill and busy dots are already gated
 * on an active session existing).
 */
export class ChatToolbar extends AntiHookComponent<{}, IChatToolbarState> {
  public state: IChatToolbarState = { modal: null, showRoleModels: false, moreOpen: false };

  // Detached, structurally shared view: ModelSelector and the modals get a plain snapshot.
  private readonly activeSessionView = this.connectSelection(sessionsCarburetor, selectActiveSession);

  /** Opens or closes the More menu. */
  @bind
  protected toggleMore(): void {
    this.setState(flipMoreOpen);
  }

  /** Closes the More menu. */
  @bind
  protected closeMore(): void {
    this.setState({ moreOpen: false });
  }

  /**
   * Opens a modal picked from the More menu.
   *
   * @param modal - which modal
   */
  @bind
  protected openModal(modal: ToolbarModal): void {
    this.setState({ modal });
  }

  /** Closes the open modal. */
  @bind
  protected closeModal(): void {
    this.setState({ modal: null });
  }

  /** Shows or hides the worker/reviewer selectors. */
  @bind
  protected toggleRoleModels(): void {
    this.setState(flipRoleModels);
  }

  /** Renders the toolbar. */
  public render() {
    const { modal, showRoleModels, moreOpen } = this.state;
    const { activeSessionID } = this.useCarburetor(sessionsCarburetor);
    const { busySessions, summarizeQueued } = this.useCarburetor(busyCarburetor);
    const { name: modelName, contextWindow } = this.useComputed(activeSmartModel);
    const activeSession = this.activeSessionView();
    const isBusy = activeSessionID ? busySessions.includes(activeSessionID) : false;
    const isQueued = activeSessionID ? summarizeQueued.includes(activeSessionID) : false;
    const hasMessages = (activeSession?.MessageCount ?? 0) > 0;
    const totalTokens = activeSession ? activeSession.PromptTokens + activeSession.CompletionTokens : 0;
    const contextPct =
      contextWindow > 0 ? Math.min(100, Math.round((totalTokens / contextWindow) * 100)) : null;
    // Read-only follow mode: another live rush process holds this session's
    // lock. The toolbar still renders so the operator can see the token
    // counter, status, etc., but every mutation control collapses to a
    // single inline notice so we don't fight the foreign agent.
    if (activeSession?.OwnedExternal) {
      return (
        <div className="px-5 pt-3 pb-1 border-t border-surface bg-canvas shrink-0 flex items-center gap-2 flex-wrap">
          {totalTokens > 0 && (
            <TokenPill
              totalTokens={totalTokens}
              contextPct={contextPct}
              title={`${totalTokens.toLocaleString()} tokens`}
              summarized={false}
            />
          )}
          {isBusy && <BusyDots modelName={modelName} />}
          <div className="flex-1" />
          <StatusBar inline />
        </div>
      );
    }

    const tokenTitle =
      `${totalTokens.toLocaleString()} tokens total across all requests in this session ` +
      "(includes system prompt + tool definitions sent each turn)" +
      (contextWindow > 0 ? ` · context window: ${contextWindow.toLocaleString()}` : "");

    return (
      <>
        <div className="px-5 pt-3 pb-1 border-t border-surface bg-canvas shrink-0 flex items-center gap-2 flex-wrap">
          {/* LEFT cluster — tokens + Compact sit together (Compact directly
              operates on the context window the token pill displays) followed
              by the migrated header buttons. */}
          {activeSession && totalTokens > 0 && (
            <TokenPill
              totalTokens={totalTokens}
              contextPct={contextPct}
              title={tokenTitle}
              summarized={!!activeSession.SummaryMessageID}
            />
          )}

          {/* Compact operates on the active session's history — hidden with
              no session selected. */}
          {activeSessionID && (
            <CompactButton
              sessionID={activeSessionID}
              isQueued={isQueued}
              isBusy={isBusy}
              hasMessages={hasMessages}
            />
          )}

          <CollapseAllButton />

          <MoreMenu
            open={moreOpen}
            hasSession={!!activeSessionID}
            onToggle={this.toggleMore}
            onClose={this.closeMore}
            onPick={this.openModal}
          />

          <UpdateBadge />
          <ThemeToggle />
          <KeepAliveToggle />
          <SitterPill />
          {isBusy && <BusyDots modelName={modelName} />}

          {/* Push the right cluster to the far edge. */}
          <div className="flex-1" />

          {/* RIGHT cluster */}
          <ModelCluster
            activeSession={activeSession}
            showRoleModels={showRoleModels}
            onToggleRoleModels={this.toggleRoleModels}
          />
        </div>

        {/* Modal hosts */}
        <ToolbarModals
          modal={modal}
          activeSessionID={activeSessionID}
          activeSession={activeSession}
          onClose={this.closeModal}
        />
      </>
    );
  }
}
