import type { ChangeEvent, RefObject } from "react";
import { AntiHookComponent } from "@react-carburetor/core";
import { History, ListOrdered, Paperclip, PlusCircle, Send, SendHorizonal, Zap } from "lucide-react";

/** Props of the composer button cluster. */
interface IComposerActionsProps {
  hasSession: boolean;
  agentBusy: boolean;
  canSend: boolean;
  historyOpen: boolean;
  /** True when there is no session or no prompt to recall. */
  historyDisabled: boolean;
  fileInputRef: RefObject<HTMLInputElement | null>;
  onFileChange: (e: ChangeEvent<HTMLInputElement>) => void;
  onToggleHistory: () => void;
  onPickFile: () => void;
  onSendFast: () => void;
  onInterrupt: () => void;
  onInject: () => void;
  onSend: () => void;
}

/** Leading classes of the Interrupt and Inject buttons. */
const INTERRUPT_CLASS = "font-bold rounded-xl px-4 py-2.5";

/** Colours of the Interrupt button. */
const INTERRUPT_TONE = "bg-yellow/15 border border-yellow/40 text-yellow hover:bg-yellow/25";

/** Colours of the Inject button. */
const INJECT_TONE = "bg-green/15 border border-green/40 text-green hover:bg-green/25";

/** Trailing classes shared by the Interrupt / Inject / Send buttons. */
const ACTION_TAIL = "text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2";

/** The row of composer buttons: history, attach, fast send, interrupt, inject and send/queue. */
export class ComposerActions extends AntiHookComponent<IComposerActionsProps> {
  /** Renders the file input and the buttons that apply to the current state. */
  public render() {
    const { hasSession, agentBusy, canSend, historyOpen, historyDisabled } = this.props;
    return (
      <div className="flex items-center gap-2 shrink-0">
        <input
          ref={this.props.fileInputRef}
          type="file"
          multiple
          className="hidden"
          onChange={this.props.onFileChange}
          aria-label="Attach files"
        />
        <button
          onClick={this.props.onToggleHistory}
          disabled={historyDisabled}
          title="Your previous prompts in this session (↑ in input also works)"
          className={`btn-input-action ${historyOpen ? "text-accent bg-accent/10" : ""}`}
          data-test-id="chat-input-history-button"
        >
          <History size={18} />
        </button>
        {!agentBusy && (
          <button
            onClick={this.props.onPickFile}
            disabled={!hasSession}
            title="Attach files"
            className="btn-input-action"
          >
            <Paperclip size={18} />
          </button>
        )}
        {!agentBusy && (
          <button
            onClick={this.props.onSendFast}
            disabled={!canSend}
            title="Send with lightweight model"
            className="btn-input-action"
          >
            <SendHorizonal size={18} />
          </button>
        )}
        {agentBusy && (
          <button
            onClick={this.props.onInterrupt}
            disabled={!canSend}
            data-test-id="chat-input-interrupt-button"
            title={
              "Cancel the current turn and submit this message immediately. " +
              "The partial assistant reply stays in history."
            }
            className={`${INTERRUPT_CLASS} ${ACTION_TAIL} ${INTERRUPT_TONE}`}
          >
            <Zap size={15} />
            Interrupt
          </button>
        )}
        {agentBusy && (
          <button
            onClick={this.props.onInject}
            disabled={!canSend}
            data-test-id="chat-input-inject-button"
            title={
              "Inject this message into the running turn without cancelling. " +
              "The agent sees it in the next provider request inside the same Run()."
            }
            className={`${INTERRUPT_CLASS} ${ACTION_TAIL} ${INJECT_TONE}`}
          >
            <PlusCircle size={15} />
            Inject
          </button>
        )}
        <button
          onClick={this.props.onSend}
          disabled={!canSend}
          data-test-id="chat-input-send-button"
          className={`font-bold rounded-xl px-5 py-2.5 ${ACTION_TAIL} ${
            agentBusy
              ? "bg-base-overlay border border-surface text-text-subtle hover:border-accent/50 hover:text-text"
              : "btn-primary"
          }`}
        >
          {agentBusy ? (
            <>
              <ListOrdered size={15} />
              Queue
            </>
          ) : (
            <>
              <Send size={15} />
              Send
            </>
          )}
        </button>
      </div>
    );
  }
}
