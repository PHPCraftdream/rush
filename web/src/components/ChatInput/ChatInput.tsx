import { createRef } from "react";
import type { ChangeEvent, ClipboardEvent, DragEvent, KeyboardEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { jumpToMessage, sendWithFastModel } from "../../store";
import type { MyPromptItem, QueuedMessage, WireAttachment } from "../../store";
import { ws } from "../../ws";
import { sitterLoop } from "../../sitter/sitterLoop";
import type { SkillInfo } from "../../types";
import { isAttachmentPayloadWithinLimit } from "../../attachmentLimit";
import { busyCarburetor } from "../../state/busy";
import { connectionCarburetor } from "../../state/connection";
import { myPrompts as myPromptsComputed } from "../../state/myPrompts";
import { queueCarburetor } from "../../state/queue";
import { sessionsCarburetor } from "../../state/sessions";
import { skillsCarburetor } from "../../state/skills";
import type { ISkillsState } from "../../state/SkillsCarburetor";
import { findSession } from "../Chat/findSession";
import { AttachmentBadges } from "./AttachmentBadges";
import { ComposerActions } from "./ComposerActions";
import { ForeignOwnedBanner } from "./ForeignOwnedBanner";
import { HistoryMenu } from "./HistoryMenu";
import { OfflineBanner } from "./OfflineBanner";
import { SlashMenu } from "./SlashMenu";
import type { PendingAttachment } from "./PendingAttachment";
import { buildSendPayload } from "./buildSendPayload";
import { filterSkills } from "./filterSkills";
import { fitTextarea } from "./fitTextarea";
import { foldQueuedDraft } from "./foldQueuedDraft";
import { readFileAsBase64 } from "./readFileAsBase64";
import { slashQueryOf } from "./slashQueryOf";
import { toWireAttachments } from "./toWireAttachments";

/** Shown when the draft plus attachments would not fit one frame. */
const PAYLOAD_BUDGET_ERROR =
  "Message exceeds the 19 MiB payload budget. Shorten the text or remove attachments before sending.";

/** Shown when the queued messages plus the draft would not fit one frame. */
const QUEUE_BUDGET_ERROR =
  "Queued messages exceed the 19 MiB payload budget. Shorten the text or remove attachments before sending.";

/** Shown when a picked file alone exceeds the frame budget. */
const FILE_BUDGET_ERROR = "A file exceeds the 19 MiB payload limit. Choose a smaller file.";

/** Local state of the composer. */
interface IChatInputState {
  text: string;
  attachments: PendingAttachment[];
  attachmentError: string;
  /** -1 = live draft, 0..N-1 = a prompt from myPrompts (newest = 0). */
  histIdx: number;
  /**
   * Text the user was typing when they started recalling, so returning to
   * histIdx = -1 (or pressing Esc) restores it.
   */
  stash: string;
  /**
   * Whether the history dropdown is visible. Independent of histIdx:
   * shell-style ArrowUp/Down can recall without opening the menu.
   */
  historyOpen: boolean;
  slashOpen: boolean;
  slashIdx: number;
  /**
   * The composer only claims "offline" once a connection has been seen —
   * otherwise every fresh page load would flash the banner while the very
   * first socket is still connecting.
   */
  everConnected: boolean;
}

/**
 * Picks the skills list out of the skills state.
 *
 * @param state - the skills state
 */
const selectSkills = (state: TReadonly<ISkillsState>) => state.skills;

/**
 * Whether a keydown carries no modifier key.
 *
 * @param e - the keydown event
 */
const isBareKey = (e: KeyboardEvent<HTMLTextAreaElement>): boolean =>
  !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey;

/**
 * Text of a queued message.
 *
 * @param message - the queued message
 */
const contentOf = (message: QueuedMessage): string => message.content;

/**
 * Attachments of a queued message, or none.
 *
 * @param message - the queued message
 */
const attachmentsOf = (message: QueuedMessage): WireAttachment[] => message.attachments ?? [];

/**
 * Size record of a picked file.
 *
 * @param file - the file
 */
const sizeOf = (file: File): { size: number } => ({ size: file.size });

/**
 * Whether a picked file alone exceeds the frame budget.
 *
 * @param file - the file's size record
 * @param file.size - bytes
 */
const exceedsBudget = (file: { size: number }): boolean => !isAttachmentPayloadWithinLimit("", [file]);

/**
 * Flips the history dropdown.
 *
 * @param s - the composer state
 */
const flipHistoryOpen = (s: IChatInputState): Pick<IChatInputState, "historyOpen"> => ({
  historyOpen: !s.historyOpen,
});

/**
 * Moves the history selection towards newer prompts, stopping at the newest.
 *
 * @param s - the composer state
 */
const stepHistoryNewer = (s: IChatInputState): Pick<IChatInputState, "histIdx"> => ({
  histIdx: Math.max(s.histIdx - 1, 0),
});

/**
 * Moves the slash selection up, stopping at the first row.
 *
 * @param s - the composer state
 */
const stepSlashUp = (s: IChatInputState): Pick<IChatInputState, "slashIdx"> => ({
  slashIdx: Math.max(s.slashIdx - 1, 0),
});

/**
 * The active session id, read from the store at call time.
 */
const currentSessionID = (): string | null => sessionsCarburetor.getData().activeSessionID;

/**
 * The composer: draft, attachments, slash-command menu, prompt history and the send family.
 *
 * Reads the store at call time inside the send handlers, so a handler always acts on the
 * freshest session, busy flag and queue.
 */
export class ChatInput extends AntiHookComponent<{}, IChatInputState> {
  public state: IChatInputState = {
    text: "",
    attachments: [],
    attachmentError: "",
    histIdx: -1,
    stash: "",
    historyOpen: false,
    slashOpen: false,
    slashIdx: 0,
    everConnected: false,
  };

  private readonly textareaRef = createRef<HTMLTextAreaElement>();

  private readonly fileInputRef = createRef<HTMLInputElement>();

  private readonly skillsView = this.connectSelection(skillsCarburetor, selectSkills);

  /** Declares the session-switch resets, the offline latch and the slash-menu auto-open. */
  protected useEffects(): void {
    super.useEffects();
    const activeSessionID = currentSessionID();
    const slashQuery = slashQueryOf(this.state.text);
    const { skills, lastUsedSkill } = skillsCarburetor.getData();
    this.useEffect(this.resetRecall, [activeSessionID]);
    this.useEffect(this.focusTextarea, [activeSessionID]);
    this.useEffect(this.latchEverConnected, [connectionCarburetor.getData().connected]);
    this.useEffect(this.syncSlashMenu, [slashQuery, filterSkills(skills, slashQuery).length, lastUsedSkill]);
  }

  /** Reset recall when the session changes — the prompts list is per-session. */
  @bind
  protected resetRecall(): void {
    const { histIdx, stash, historyOpen } = this.state;
    if (histIdx === -1 && stash === "" && !historyOpen) return;
    this.setState({ histIdx: -1, stash: "", historyOpen: false });
  }

  /** Focuses the textarea once a session is active. */
  @bind
  protected focusTextarea(): void {
    if (currentSessionID() && this.textareaRef.current) {
      this.textareaRef.current.focus();
    }
  }

  /** Remembers that a connection has been seen. */
  @bind
  protected latchEverConnected(): void {
    if (connectionCarburetor.getData().connected && !this.state.everConnected) {
      this.setState({ everConnected: true });
    }
  }

  /** Auto-opens the slash menu and pre-selects: last used skill, else best name match, else first. */
  @bind
  protected syncSlashMenu(): void {
    const slashQuery = slashQueryOf(this.state.text);
    const { skills, lastUsedSkill } = skillsCarburetor.getData();
    const filtered = filterSkills(skills, slashQuery);
    if (slashQuery !== null && filtered.length > 0) {
      const lastIdx = filtered.findIndex((s) => s.name === lastUsedSkill);
      if (lastIdx >= 0) {
        this.openSlashMenu(lastIdx);
      } else {
        // Exact prefix match ranks first
        const q = slashQuery.toLowerCase();
        const exactIdx = filtered.findIndex((s) => s.name.toLowerCase().startsWith(q));
        this.openSlashMenu(exactIdx >= 0 ? exactIdx : 0);
      }
    } else if (this.state.slashOpen) {
      this.setState({ slashOpen: false });
    }
  }

  /**
   * Opens the slash menu with a row pre-selected; a no-op when it already shows that.
   *
   * @param idx - the row to pre-select
   */
  protected openSlashMenu(idx: number): void {
    if (this.state.slashOpen && this.state.slashIdx === idx) return;
    this.setState({ slashOpen: true, slashIdx: idx });
  }

  /** Collapses the textarea back to one row. */
  protected resetTextareaHeight(): void {
    if (this.textareaRef.current) this.textareaRef.current.style.height = "auto";
  }

  /** Clears the draft, its attachments and the recall state after a successful send. */
  protected resetDraft(): void {
    this.setState({ attachments: [], text: "", histIdx: -1, stash: "", historyOpen: false });
    this.resetTextareaHeight();
  }

  /** Parks the caret at the end of the textarea and focuses it. */
  @bind
  protected focusEnd(): void {
    const el = this.textareaRef.current;
    if (!el) return;
    el.focus();
    const len = el.value.length;
    el.setSelectionRange(len, len);
  }

  /** Resizes the textarea to its recalled content and parks the caret at the very end. */
  @bind
  protected fitAndFocusRecalled(): void {
    const el = this.textareaRef.current;
    if (!el) return;
    fitTextarea(el);
    this.focusEnd();
  }

  /**
   * Picks a skill from the slash menu: Tab fills the command in, Enter sends it right away.
   *
   * @param skill - the chosen skill
   * @param sendNow - true to send its instructions immediately
   */
  @bind
  protected handleSkillSelect(skill: SkillInfo, sendNow: boolean): void {
    skillsCarburetor.setLastUsedSkill(skill.name);
    this.setState({ slashOpen: false });
    if (!sendNow) {
      // Tab: fill in the command name + space so user can keep typing
      this.setState({ text: `/${skill.name} ` });
      setTimeout(this.focusEnd, 0);
      return;
    }
    const sessionID = currentSessionID();
    if (!sessionID) return;
    const content = skill.instructions || `/${skill.name}`;
    const { attachments } = this.state;
    if (!isAttachmentPayloadWithinLimit(content, attachments)) {
      this.setState({ attachmentError: PAYLOAD_BUDGET_ERROR });
      return;
    }
    this.setState({ attachmentError: "" });
    // A file picked before the skill was chosen rides on the same frame
    // (same wire shape as send(): the backend appends "[Attached file:
    // <path>]" to the prompt itself) — without this the badge would stay
    // visible while the file silently never goes out.
    const payload = buildSendPayload(sessionID, content, toWireAttachments(attachments));
    // Park in the offline outbox when the socket is down; only clear the
    // input once the frame is on the wire or queued for delivery.
    if (!ws.sendQueued("send_message", payload)) return;
    this.setState({ text: "", attachments: [] });
    this.resetTextareaHeight();
  }

  /**
   * Intercepts the `/sitter` family before any server roundtrip — it is a
   * pure-client toggle (start/stop the auto-resume loop). Other slash
   * commands flow through unchanged.
   *
   * @param msg - the trimmed draft
   */
  protected trySitterCommand(msg: string): boolean {
    if (!msg || !sitterLoop.handleCommand(msg)) return false;
    this.setState({ text: "", histIdx: -1, stash: "", historyOpen: false });
    this.resetTextareaHeight();
    return true;
  }

  /** Sends the draft, or queues it while the agent is busy. */
  @bind
  protected send(): void {
    const { text, attachments } = this.state;
    const msg = text.trim();
    const sessionID = currentSessionID();
    if (!sessionID) return;
    if (!isAttachmentPayloadWithinLimit(msg, attachments)) {
      this.setState({ attachmentError: PAYLOAD_BUDGET_ERROR });
      return;
    }
    this.setState({ attachmentError: "" });
    // Attachment-only sends are legal: the backend's send_message accepts
    // Attachments independent of Content (handlers_agent.go appends
    // "[Attached file: <path>]" to the prompt itself), so a pending
    // attachment with an empty draft still means "send".
    // Slash-command decision: command interception is deliberately
    // text-only — commands are parsed from the typed text, and an
    // attachment-only send has no text, hence no command to interpret.
    // It goes out as a raw frame with empty content.
    if (!msg && attachments.length === 0) return;
    if (this.trySitterCommand(msg)) return;
    if (busyCarburetor.isBusy(sessionID)) {
      const queuedMessages = queueCarburetor.getData().messageQueue[sessionID] ?? [];
      const aggregateContent = [...queuedMessages.map(contentOf), msg].join("\n\n");
      const aggregateAttachments = [
        ...queuedMessages.flatMap(attachmentsOf),
        ...toWireAttachments(attachments),
      ];
      if (!isAttachmentPayloadWithinLimit(aggregateContent, aggregateAttachments)) {
        this.setState({ attachmentError: QUEUE_BUDGET_ERROR });
        return;
      }
      // The attachment is captured by the queued message — it will go out
      // with the flushed send, so clearing the badge here is honest.
      queueCarburetor.enqueue(sessionID, msg, toWireAttachments(attachments));
    } else {
      // When the socket is down the frame is parked in the offline outbox
      // and flushed on reconnect; if even that failed (outbox full) keep
      // the draft so the composer can't silently swallow the message.
      const payload = buildSendPayload(sessionID, msg, toWireAttachments(attachments));
      if (!ws.sendQueued("send_message", payload)) return;
    }
    this.resetDraft();
  }

  /** Sends the draft with the lightweight model. */
  @bind
  protected sendFast(): void {
    const { text, attachments } = this.state;
    const msg = text.trim();
    const sessionID = currentSessionID();
    // Attachment-only fast-sends are legal for the same reason as send().
    if (!sessionID || busyCarburetor.isBusy(sessionID)) return;
    if (!isAttachmentPayloadWithinLimit(msg, attachments)) {
      this.setState({ attachmentError: PAYLOAD_BUDGET_ERROR });
      return;
    }
    this.setState({ attachmentError: "" });
    if (!msg && attachments.length === 0) return;
    // sendWithFastModel fire-and-forgets its frame internally (store.ts), so
    // guard here: with the socket down, keep the draft rather than let it
    // vanish into a no-op send.
    if (!ws.isOpen()) return;
    sendWithFastModel(sessionID, msg, toWireAttachments(attachments));
    this.resetDraft();
  }

  /**
   * Interrupts the in-flight turn AND submits the draft immediately.
   *
   * The server's interrupt_and_send handler queues the new message and cancels the
   * running turn; agent.Run()'s cancel-handling branch drains the queue
   * and re-enters Run() with this message — so the partial assistant
   * output stays in history and the new instruction picks up right away.
   * Anything in the local front-end queue (text AND attachments) is folded
   * into the submitted frame, otherwise it would race the cancel and arrive
   * in a separate send_message after the interrupt-turn finishes.
   */
  @bind
  protected interrupt(): void {
    const sessionID = currentSessionID();
    if (!sessionID) return;
    const queuedMessages = queueCarburetor.getData().messageQueue[sessionID] ?? [];
    const { content, atts, hadQueued } = foldQueuedDraft(queuedMessages, this.state.text, this.state.attachments);
    if (!isAttachmentPayloadWithinLimit(content, atts)) {
      this.setState({ attachmentError: PAYLOAD_BUDGET_ERROR });
      return;
    }
    this.setState({ attachmentError: "" });
    // Attachment-only is valid (see send()) — the backend appends the saved
    // attachment paths to the prompt itself. Without this, relaxing canSend
    // would leave the Interrupt button enabled but dead.
    if (!content && atts.length === 0) return;
    const payload = buildSendPayload(sessionID, content, atts);
    // Held in the offline outbox when the socket is down (the local queue
    // was already folded into this payload, so parking the whole frame is
    // the only way to not lose it) and delivered on reconnect.
    // Stale-action policy (task #726): parked as interrupt_and_send, NOT
    // rewritten to send_message. If the target turn already finished
    // server-side by delivery time, the server's idle path degrades the
    // interrupt into exactly the desired default — a fresh run of the
    // content (coordinator.InterruptAndSend → startDetachedRun durable
    // enqueue); if the turn is still running, cancel-and-replace happens
    // as the user asked. Both delivery worlds are safe, so the frame
    // keeps its original type.
    if (!ws.sendQueued("interrupt_and_send", payload)) return;
    if (hadQueued) queueCarburetor.dequeueAll(sessionID);
    this.resetDraft();
  }

  /**
   * Injects the draft into the running turn WITHOUT cancelling it.
   *
   * Backend persists the user message to the DB immediately (UI updates via
   * the same pubsub path as a regular send_message) and schedules it to be
   * appended to prepared.Messages at the next PrepareStep — so the model
   * sees it on its next provider request inside the SAME Run(). Anything in
   * the front-end local queue (both text AND attachments) is folded in so
   * we don't race.
   */
  @bind
  protected inject(): void {
    const sessionID = currentSessionID();
    if (!sessionID) return;
    const queuedMessages = queueCarburetor.getData().messageQueue[sessionID] ?? [];
    const { content, atts, hadQueued } = foldQueuedDraft(queuedMessages, this.state.text, this.state.attachments);
    if (!isAttachmentPayloadWithinLimit(content, atts)) {
      this.setState({ attachmentError: PAYLOAD_BUDGET_ERROR });
      return;
    }
    this.setState({ attachmentError: "" });
    // Attachment-only is valid (see send()) — same reasoning as interrupt.
    if (!content && atts.length === 0) return;
    const payload = buildSendPayload(sessionID, content, atts);
    // Same offline-outbox treatment as interrupt: the dequeued local queue
    // lives in this frame now, so it must be parked or sent, never dropped.
    // Stale-action policy (task #726): "inject" means "merge into the
    // RUNNING turn", but while the socket is down we cannot know whether
    // that turn still exists when the frame is finally delivered. The
    // server's idle-inject semantics would only persist the message and
    // leave it unanswered until some later turn happens to run (see
    // sessionAgent.InjectMessage's injectIfBusy no-op path). So an inject
    // parked offline is re-shaped into a plain send_message, which is
    // correct in both worlds: if the turn ended, it starts a fresh turn
    // (immediate answer); if the turn is still running, the server queues
    // the call as the next turn's content (handleSendMessage on an owned
    // session). Nothing stays dormant. sendQueued keeps whatever type it's
    // given whether it sends immediately or parks (see ws.ts), so the type
    // must be chosen HERE based on the socket's current state — the normal
    // online case still needs the real inject_message type.
    const type = ws.isOpen() ? "inject_message" : "send_message";
    if (!ws.sendQueued(type, payload)) return;
    if (hadQueued) queueCarburetor.dequeueAll(sessionID);
    this.resetDraft();
  }

  /**
   * Applies the prompt at index `idx` to the textarea. idx=-1 restores the
   * user's draft from `stash`. Resizes the textarea and parks the caret
   * at the very end, matching what a user expects from "the previous prompt".
   *
   * @param idx - index into the prompt history, or -1 for the live draft
   */
  protected applyRecall(idx: number): void {
    const next = idx === -1 ? this.state.stash : (myPromptsComputed.get()[idx]?.text ?? "");
    this.setState({ histIdx: idx, text: next });
    requestAnimationFrame(this.fitAndFocusRecalled);
  }

  /** Recalls the next older prompt, stashing the live draft first. */
  protected recallPrev(): void {
    const prompts = myPromptsComputed.get();
    if (prompts.length === 0) return;
    const { histIdx, text } = this.state;
    if (histIdx === -1) this.setState({ stash: text });
    const next = Math.min(histIdx + 1, prompts.length - 1);
    if (next === histIdx) return;
    this.applyRecall(next);
  }

  /** Recalls the next newer prompt, or the live draft past the newest. */
  protected recallNext(): void {
    const { histIdx } = this.state;
    if (histIdx === -1) return; // already at live draft
    this.applyRecall(histIdx - 1);
  }

  /**
   * Whether the caret is visually on the first line of the textarea.
   *
   * True if there is no newline before the caret. This is what lets
   * ArrowUp/Down move the caret normally on multi-line inputs and ONLY
   * trigger recall at the line edges.
   */
  protected caretOnFirstLine(): boolean {
    const el = this.textareaRef.current;
    if (!el) return true;
    return el.value.slice(0, el.selectionStart).indexOf("\n") === -1;
  }

  /** True if there is no newline after the caret (visually on the last line). */
  protected caretOnLastLine(): boolean {
    const el = this.textareaRef.current;
    if (!el) return true;
    return el.value.slice(el.selectionEnd).indexOf("\n") === -1;
  }

  /**
   * Recalls a prompt picked from the history menu.
   *
   * @param item - the picked prompt
   */
  @bind
  protected handleRecallFromMenu(item: MyPromptItem): void {
    const idx = myPromptsComputed.get().findIndex((p) => p.id === item.id);
    if (idx < 0) return;
    const { histIdx, text } = this.state;
    if (histIdx === -1) this.setState({ stash: text });
    this.setState({ historyOpen: false });
    this.applyRecall(idx);
  }

  /**
   * Jumps the transcript to a prompt, leaving the draft alone.
   *
   * @param item - the picked prompt
   */
  @bind
  protected handleJumpFromMenu(item: MyPromptItem): void {
    this.setState({ historyOpen: false });
    jumpToMessage(item.id);
  }

  /** Opens or closes the history dropdown. */
  @bind
  protected toggleHistoryMenu(): void {
    this.setState(flipHistoryOpen);
  }

  /**
   * History menu walks the same Up/Down/Enter/Esc as the slash menu but
   * takes priority when open; selection moves through myPrompts.
   *
   * @param e - the keydown event
   * @returns whether the key was consumed
   */
  protected handleHistoryMenuKey(e: KeyboardEvent<HTMLTextAreaElement>): boolean {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      this.setState(stepHistoryNewer);
      return true;
    }
    if (e.key === "ArrowUp") {
      const last = Math.max(myPromptsComputed.get().length - 1, 0);
      e.preventDefault();
      this.setState((s) => ({ histIdx: Math.min(s.histIdx + 1, last) }));
      return true;
    }
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      const item = myPromptsComputed.get()[this.state.histIdx === -1 ? 0 : this.state.histIdx];
      if (item) this.handleRecallFromMenu(item);
      return true;
    }
    if (e.key === "Escape") {
      e.preventDefault();
      this.setState({ historyOpen: false });
      return true;
    }
    return false;
  }

  /**
   * Slash menu navigation: arrows move, Tab fills, Enter sends, Esc closes.
   *
   * @param e - the keydown event
   * @param filtered - the skills currently on offer
   * @returns whether the key was consumed
   */
  protected handleSlashMenuKey(e: KeyboardEvent<HTMLTextAreaElement>, filtered: readonly SkillInfo[]): boolean {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      this.setState((s) => ({ slashIdx: Math.min(s.slashIdx + 1, filtered.length - 1) }));
      return true;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      this.setState(stepSlashUp);
      return true;
    }
    if (e.key === "Tab") {
      e.preventDefault();
      this.handleSkillSelect(filtered[this.state.slashIdx], false);
      return true;
    }
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      this.handleSkillSelect(filtered[this.state.slashIdx], true);
      return true;
    }
    if (e.key === "Escape") {
      e.preventDefault();
      this.setState({ slashOpen: false });
      return true;
    }
    return false;
  }

  /**
   * Shell-style recall on bare ArrowUp/Down (no menu, no modifiers). Only
   * fires at the visual line edge so multi-line editing isn't disturbed.
   * Esc while recalling restores the user's original draft from the stash.
   *
   * @param e - the keydown event
   * @returns whether the key was consumed
   */
  protected handleRecallKey(e: KeyboardEvent<HTMLTextAreaElement>): boolean {
    const { histIdx } = this.state;
    if (e.key === "ArrowUp" && isBareKey(e)) {
      if (this.caretOnFirstLine() && myPromptsComputed.get().length > 0) {
        e.preventDefault();
        this.recallPrev();
        return true;
      }
    }
    if (e.key === "ArrowDown" && isBareKey(e)) {
      if (this.caretOnLastLine() && histIdx !== -1) {
        e.preventDefault();
        this.recallNext();
        return true;
      }
    }
    if (e.key === "Escape" && histIdx !== -1) {
      e.preventDefault();
      this.applyRecall(-1);
      this.setState({ stash: "" });
      return true;
    }
    return false;
  }

  /**
   * Routes composer keys: history menu, slash menu, shell-style recall, then Enter to send.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent<HTMLTextAreaElement>): void {
    if (this.state.historyOpen && this.handleHistoryMenuKey(e)) return;
    const filtered = filterSkills(skillsCarburetor.getData().skills, slashQueryOf(this.state.text));
    if (this.state.slashOpen && filtered.length > 0 && this.handleSlashMenuKey(e, filtered)) return;
    if (this.handleRecallKey(e)) return;
    if (e.key === "Enter" && !e.shiftKey && !e.ctrlKey) {
      e.preventDefault();
      this.send();
    }
  }

  /**
   * Tracks the draft and grows the textarea.
   *
   * @param e - the change event
   */
  @bind
  protected onInput(e: ChangeEvent<HTMLTextAreaElement>): void {
    this.setState({ text: e.target.value });
    // The moment the user edits a recalled prompt, treat it as a fresh draft:
    // arrow-up should walk history again from "the current text" downwards.
    if (this.state.histIdx !== -1) {
      this.setState({ histIdx: -1, stash: "" });
    }
    fitTextarea(e.target);
  }

  /**
   * Adds files as pending attachments after checking each against the frame budget.
   *
   * @param files - the picked, dropped or pasted files
   */
  protected async appendAttachments(files: File[]): Promise<void> {
    if (files.length === 0) return;
    if (files.map(sizeOf).some(exceedsBudget)) {
      this.setState({ attachmentError: FILE_BUDGET_ERROR });
      return;
    }
    this.setState({ attachmentError: "" });
    const reads = await Promise.all(files.map(readFileAsBase64));
    this.setState((s) => ({ attachments: [...s.attachments, ...reads] }));
  }

  /**
   * Adds the files chosen in the file dialog, then resets the input so the same file can be picked again.
   *
   * @param e - the change event of the file input
   */
  @bind
  protected async onFileChange(e: ChangeEvent<HTMLInputElement>): Promise<void> {
    const files = Array.from(e.target.files ?? []);
    await this.appendAttachments(files);
    if (this.fileInputRef.current) this.fileInputRef.current.value = "";
  }

  /** Opens the file dialog. */
  @bind
  protected pickFile(): void {
    this.fileInputRef.current?.click();
  }

  /**
   * Drops one pending attachment.
   *
   * @param idx - its position
   */
  @bind
  protected removeAttachment(idx: number): void {
    this.setState((s) => ({ attachments: s.attachments.filter((_, i) => i !== idx), attachmentError: "" }));
  }

  /**
   * Allows dropping files on the composer.
   *
   * @param e - the dragover event
   */
  @bind
  protected onDragOver(e: DragEvent): void {
    e.preventDefault();
  }

  /**
   * Adds dropped files as attachments.
   *
   * @param e - the drop event
   */
  @bind
  protected async onDrop(e: DragEvent): Promise<void> {
    e.preventDefault();
    const files = Array.from(e.dataTransfer.files);
    if (files.length === 0) return;
    await this.appendAttachments(files);
  }

  /**
   * Adds pasted files as attachments; plain-text pastes fall through to the textarea.
   *
   * @param e - the paste event
   */
  @bind
  protected async onPaste(e: ClipboardEvent): Promise<void> {
    const items = Array.from(e.clipboardData.items);
    const pastedFiles: File[] = [];
    for (const item of items) {
      if (item.kind === "file") {
        const file = item.getAsFile();
        if (file) pastedFiles.push(file);
      }
    }
    if (pastedFiles.length === 0) return;
    e.preventDefault();
    await this.appendAttachments(pastedFiles);
  }

  /** Renders the composer, or the read-only banner for a session another process drives. */
  public render() {
    const { text, attachments, attachmentError, histIdx, historyOpen, slashOpen, slashIdx, everConnected } = this.state;
    const { activeSessionID, sessions } = this.useCarburetor(sessionsCarburetor);
    const { busySessions } = this.useCarburetor(busyCarburetor);
    const { connected, outboxCount } = this.useCarburetor(connectionCarburetor);
    // Subscribes the composer to the last-used skill: the slash-menu effect re-evaluates when it changes.
    void this.useCarburetor(skillsCarburetor).lastUsedSkill;
    const promptCount = this.useComputed(myPromptsComputed).length;
    const filteredSkills = filterSkills(this.skillsView(), slashQueryOf(text));
    const activeSession = findSession(sessions, activeSessionID);
    if (activeSession?.OwnedExternal) return <ForeignOwnedBanner ownerPID={activeSession.OwnedByPID ?? 0} />;

    const agentBusy = activeSessionID ? busySessions.includes(activeSessionID) : false;
    const offline = everConnected && !connected;
    // A send needs text OR a pending attachment — the backend accepts
    // attachment-only frames (content goes out empty).
    const canSend = (!!text.trim() || attachments.length > 0) && !!activeSessionID;
    let placeholder = "Select or create a session";
    if (activeSessionID) {
      placeholder = agentBusy ? "Message… (will be queued)" : "Message… (/ for skills, Enter to send)";
    }

    return (
      <div className="px-5 pt-2 pb-4 bg-canvas shrink-0">
        {offline && <OfflineBanner outboxCount={outboxCount} />}
        {attachmentError && (
          <div role="alert" className="mb-2 text-xs text-red-400">{attachmentError}</div>
        )}
        {attachments.length > 0 && <AttachmentBadges attachments={attachments} onRemove={this.removeAttachment} />}

        <div className="relative">
          {/* Slash command dropdown — opens above input */}
          {slashOpen && <SlashMenu items={filteredSkills} selectedIdx={slashIdx} onSelect={this.handleSkillSelect} />}

          {/* History dropdown — recall + jump for the user's own prompts. */}
          {historyOpen && !slashOpen && (
            <HistoryMenu
              selectedIdx={histIdx === -1 ? 0 : histIdx}
              onRecall={this.handleRecallFromMenu}
              onJump={this.handleJumpFromMenu}
            />
          )}

          <div
            className={`flex gap-4 items-end bg-base-overlay border rounded-2xl px-5 py-4 transition-all ${
              activeSessionID
                ? "border-surface focus-within:border-accent/50 focus-within:shadow-md focus-within:bg-canvas"
                : "border-surface opacity-60"
            }`}
            onDragOver={this.onDragOver}
            onDrop={this.onDrop}
          >
            <textarea
              ref={this.textareaRef}
              value={text}
              onChange={this.onInput}
              onKeyDown={this.onKey}
              onPaste={this.onPaste}
              placeholder={placeholder}
              disabled={!activeSessionID}
              autoFocus
              rows={1}
              data-test-id="chat-input-textarea"
              className={
                "flex-1 bg-transparent border-none outline-none resize-none text-text leading-relaxed min-h-[28px] " +
                "max-h-[960px] overflow-y-auto disabled:cursor-not-allowed placeholder:text-text-subtle font-medium"
              }
              style={{ fontSize: "var(--chat-font-size)" }}
            />
            <ComposerActions
              hasSession={!!activeSessionID}
              agentBusy={agentBusy}
              canSend={canSend}
              historyOpen={historyOpen}
              historyDisabled={!activeSessionID || promptCount === 0}
              fileInputRef={this.fileInputRef}
              onFileChange={this.onFileChange}
              onToggleHistory={this.toggleHistoryMenu}
              onPickFile={this.pickFile}
              onSendFast={this.sendFast}
              onInterrupt={this.interrupt}
              onInject={this.inject}
              onSend={this.send}
            />
          </div>
        </div>
      </div>
    );
  }
}
