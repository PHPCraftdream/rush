import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { deleteMessage, deleteMessages, rerunFromMessage } from "../../store";
import type { Todo } from "../../types";
import { asyncJobCompletions } from "../../state/asyncJobCompletions";
import { busyCarburetor } from "../../state/busy";
import { messagesCarburetor } from "../../state/messages";
import type { IMessagesState } from "../../state/MessagesCarburetor";
import { noticesCarburetor } from "../../state/notices";
import { queueCarburetor } from "../../state/queue";
import type { IQueueState } from "../../state/QueueCarburetor";
import { selectionCarburetor } from "../../state/selection";
import { sessionsCarburetor } from "../../state/sessions";
import type { ISessionsState } from "../../state/SessionsCarburetor";
import { ChatInput } from "../ChatInput";
import { ChatToolbar } from "../ChatToolbar";
import { ConfirmDialog } from "../ConfirmDialog";
import { LiveWorkPanel } from "../LiveWorkPanel";
import { AgentErrorBanner } from "./AgentErrorBanner";
import { BusyIndicator } from "./BusyIndicator";
import { ChatScroller } from "./ChatScroller";
import { EmptyState } from "./EmptyState";
import { QueueSection } from "./QueueSection";
import { SelectionToolbar } from "./SelectionToolbar";
import { TranscriptList } from "./TranscriptList";
import { buildRenderItems } from "./buildRenderItems";
import { clearSelection } from "./clearSelection";
import { findSession } from "./findSession";

/** What a pending confirmation does once the operator accepts it. */
type ConfirmAction =
  | { type: "deleteOne"; id: string }
  | { type: "deleteMany"; ids: string[] }
  | { type: "rerun"; id: string };

/** A pending confirmation shown in the dialog. */
interface IChatConfirm {
  title: string;
  text: string;
  confirmLabel: string;
  variant: "danger" | "warning";
  action: ConfirmAction;
}

/** Local state of the chat pane. */
interface IChatState {
  confirm: IChatConfirm | null;
}

/**
 * Picks the transcript out of the messages state.
 *
 * @param state - the messages state
 */
const selectMessages = (state: TReadonly<IMessagesState>) => state.messages;

/**
 * Picks the title of the fork the active session would produce.
 *
 * @param state - the sessions state
 */
const selectForkDefaultTitle = (state: TReadonly<ISessionsState>) =>
  (findSession(state.sessions, state.activeSessionID)?.Title || "Session") + " fork";

/**
 * Picks the active session's todos.
 *
 * @param state - the sessions state
 */
const selectActiveTodos = (state: TReadonly<ISessionsState>) =>
  findSession(state.sessions, state.activeSessionID)?.Todos ?? [];

/**
 * Picks the client message queue table out of the queue state.
 *
 * @param state - the queue state
 */
const selectMessageQueue = (state: TReadonly<IQueueState>) => state.messageQueue;

/**
 * Id of a message.
 *
 * @param m - the message
 */
const messageID = (m: { ID: string }): string => m.ID;

/**
 * Runs a confirmed action.
 *
 * @param action - what the operator accepted
 */
const runConfirmAction = (action: ConfirmAction): void => {
  if (action.type === "deleteOne") {
    deleteMessage(action.id);
    clearSelection();
  } else if (action.type === "deleteMany") {
    deleteMessages(action.ids);
    clearSelection();
  } else {
    rerunFromMessage(action.id);
  }
};

/** The chat pane: transcript, queue, selection bar, live-work panel, toolbar and composer. */
export class Chat extends AntiHookComponent<{}, IChatState> {
  public state: IChatState = { confirm: null };

  // Detached, structurally shared views: children get plain snapshots that keep their identity while unchanged.
  private readonly messagesView = this.connectSelection(messagesCarburetor, selectMessages);

  private readonly forkTitleView = this.connectSelection(sessionsCarburetor, selectForkDefaultTitle);

  private readonly todosView = this.connectSelection(sessionsCarburetor, selectActiveTodos);

  private readonly queueView = this.connectSelection(queueCarburetor, selectMessageQueue);

  /**
   * Selects a message range around the clicked one, extending the nearest selected rows.
   *
   * @param clickedIndex - index of the clicked message
   */
  @bind
  protected handleRangeSelect(clickedIndex: number): void {
    const { messages } = messagesCarburetor.getData();
    const selected = new Set(selectionCarburetor.getData().selectedMessageIDs);
    if (selected.size === 0) {
      selectionCarburetor.toggle(messages[clickedIndex].ID);
      return;
    }
    let above = -1;
    let below = -1;
    for (let i = 0; i < messages.length; i++) {
      if (selected.has(messages[i].ID)) {
        if (i < clickedIndex) above = i;
        if (i > clickedIndex && below === -1) below = i;
      }
    }
    const from = above !== -1 ? above : clickedIndex;
    const to = below !== -1 ? below : clickedIndex;
    const ids = messages.slice(Math.min(from, clickedIndex), Math.max(to, clickedIndex) + 1).map(messageID);
    selectionCarburetor.selectAll(ids);
  }

  /**
   * Selects every message from the top down to the given one.
   *
   * @param index - index of the last message to select
   */
  @bind
  protected handleSelectAbove(index: number): void {
    selectionCarburetor.selectAll(messagesCarburetor.getData().messages.slice(0, index + 1).map(messageID));
  }

  /**
   * Selects every message from the given one down to the bottom.
   *
   * @param index - index of the first message to select
   */
  @bind
  protected handleSelectBelow(index: number): void {
    selectionCarburetor.selectAll(messagesCarburetor.getData().messages.slice(index).map(messageID));
  }

  /**
   * Asks to confirm deleting one message.
   *
   * @param id - the message id
   */
  @bind
  protected requestDeleteOne(id: string): void {
    this.setState({
      confirm: {
        title: "Delete message",
        text: "Delete this message?",
        confirmLabel: "Delete",
        variant: "danger",
        action: { type: "deleteOne", id },
      },
    });
  }

  /** Asks to confirm deleting every selected message. */
  @bind
  protected requestDeleteSelected(): void {
    const ids = [...selectionCarburetor.getData().selectedMessageIDs];
    this.setState({
      confirm: {
        title: "Delete message",
        text: `Delete ${ids.length} selected message${ids.length === 1 ? "" : "s"}?`,
        confirmLabel: "Delete",
        variant: "danger",
        action: { type: "deleteMany", ids },
      },
    });
  }

  /**
   * Retries a user message, asking first when something below would be lost.
   *
   * Retry re-sends a user message: the server cancels any in-flight turn,
   * deletes the target message and everything after it, then re-runs the
   * agent with the same prompt (handleRerunMessage). Only ask for
   * confirmation when there is actually something below to lose — retrying
   * the last user message has nothing after it to delete.
   *
   * @param id - the message id
   */
  @bind
  protected requestRerun(id: string): void {
    const { messages } = messagesCarburetor.getData();
    const idx = messages.findIndex((m) => m.ID === id);
    const hasMessagesBelow = idx !== -1 && idx < messages.length - 1;
    if (!hasMessagesBelow) {
      rerunFromMessage(id);
      return;
    }
    this.setState({
      confirm: {
        title: "Retry message",
        text: "This message and everything after it will be deleted, then resent. This cannot be undone.",
        confirmLabel: "Retry",
        variant: "warning",
        action: { type: "rerun", id },
      },
    });
  }

  /** Runs the pending confirmation and closes the dialog. */
  @bind
  protected acceptConfirm(): void {
    const { confirm } = this.state;
    if (confirm) runConfirmAction(confirm.action);
    this.setState({ confirm: null });
  }

  /** Closes the dialog without acting. */
  @bind
  protected closeConfirm(): void {
    this.setState({ confirm: null });
  }

  /** Renders the pane. */
  public render() {
    const { confirm } = this.state;
    const { activeSessionID } = this.useCarburetor(sessionsCarburetor);
    const { agentError } = this.useCarburetor(noticesCarburetor);
    const { busySessions } = this.useCarburetor(busyCarburetor);
    const selectedIDs = new Set(this.useCarburetor(selectionCarburetor).selectedMessageIDs);
    const jobCompletions = this.useComputed(asyncJobCompletions);
    const messages = this.messagesView();
    const isBusy = activeSessionID ? busySessions.includes(activeSessionID) : false;
    const queuedItems = (activeSessionID ? this.queueView()[activeSessionID] : undefined) ?? [];
    // Group consecutive tool-only assistant messages into a single ToolRun so
    // a long burst of N steps renders as one container with N actions instead
    // of N near-empty per-message containers.
    const renderItems = buildRenderItems(messages, jobCompletions.attachedNoticeIDs);

    return (
      <div className="flex-1 flex flex-col overflow-hidden relative bg-canvas">
        <ChatScroller sessionID={activeSessionID} messages={messages} isBusy={isBusy} agentError={agentError}>
          {!activeSessionID ? (
            <EmptyState noSession />
          ) : messages.length === 0 && !agentError ? (
            <EmptyState noSession={false} />
          ) : (
            <TranscriptList
              renderItems={renderItems}
              sessionID={activeSessionID}
              isBusy={isBusy}
              selectedIDs={selectedIDs}
              forkDefaultTitle={this.forkTitleView()}
              onDeleteRequest={this.requestDeleteOne}
              onRerunRequest={this.requestRerun}
              onRangeSelect={this.handleRangeSelect}
              onSelectAbove={this.handleSelectAbove}
              onSelectBelow={this.handleSelectBelow}
            />
          )}
          {agentError && <AgentErrorBanner message={agentError} />}
          {isBusy && <BusyIndicator sessionID={activeSessionID} />}
        </ChatScroller>

        {queuedItems.length > 0 && activeSessionID && (
          <QueueSection items={queuedItems} sessionID={activeSessionID} sessionBusy={isBusy} />
        )}

        {selectedIDs.size > 0 && (
          <SelectionToolbar count={selectedIDs.size} onDeleteSelected={this.requestDeleteSelected} />
        )}

        {/* The cast hands the detached read-only todos to a panel that only reads them. */}
        {activeSessionID && <LiveWorkPanel sessionID={activeSessionID} todos={this.todosView() as Todo[]} />}

        <ChatToolbar />
        <ChatInput />

        {confirm && (
          <ConfirmDialog
            title={confirm.title}
            message={confirm.text}
            confirmLabel={confirm.confirmLabel}
            variant={confirm.variant}
            onConfirm={this.acceptConfirm}
            onCancel={this.closeConfirm}
          />
        )}
      </div>
    );
  }
}
