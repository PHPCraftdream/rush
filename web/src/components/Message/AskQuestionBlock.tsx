// Interactive block for an agent that stopped to ask the operator a question.

import type { ChangeEvent, FormEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import { HelpCircle, X } from "lucide-react";
import { ws } from "../../ws";

/** Props of one suggested-option chip. */
interface IOptionChipProps {
  option: string;
  onPick: (option: string) => void;
}

/** One clickable suggested answer. */
class OptionChip extends AntiHookComponent<IOptionChipProps> {
  /** Reports this chip's option as the answer. */
  @bind
  protected pick(): void {
    this.props.onPick(this.props.option);
  }

  /** Renders the chip. */
  public render() {
    return (
      <button
        type="button"
        onClick={this.pick}
        className="px-3 py-1.5 text-sm rounded-full border border-accent/40 text-accent hover:bg-accent/15 active:scale-[0.97] transition-all"
      >
        {this.props.option}
      </button>
    );
  }
}

/** Props of the question block. */
interface IAskQuestionBlockProps {
  question: string;
  options: string[];
  sessionID: string;
}

/** Local state of the question block. */
interface IAskQuestionBlockState {
  dismissed: boolean;
  answer: string;
}

/**
 * The agent is waiting for an answer. Renders the question as plain text
 * (matches how a normal reply reads), a row of clickable chips for any
 * suggested options, and an ALWAYS-VISIBLE inline free-text input + send button
 * (explicitly requested: don't rely on the operator noticing they can just type
 * in the chat box below). Both paths send through the normal chat pipeline — no
 * new backend state, no special "answered" flag. "Dismiss" only hides this
 * block locally; it sends nothing and does not mark anything answered server-side.
 */
export class AskQuestionBlock extends AntiHookComponent<IAskQuestionBlockProps, IAskQuestionBlockState> {
  public state: IAskQuestionBlockState = { dismissed: false, answer: "" };

  /**
   * Sends a trimmed, non-empty answer and hides the block.
   *
   * @param text - the answer text
   */
  @bind
  protected answerWith(text: string): void {
    const { sessionID } = this.props;
    const trimmed = text.trim();
    if (!trimmed || !sessionID) return;
    ws.send("send_message", { sessionID, content: trimmed });
    this.setState({ dismissed: true });
  }

  /**
   * Submits the free-text answer.
   *
   * @param e - the form submit event
   */
  @bind
  protected onSubmit(e: FormEvent): void {
    e.preventDefault();
    this.answerWith(this.state.answer);
  }

  /**
   * Tracks the typed answer.
   *
   * @param e - the input change event
   */
  @bind
  protected onAnswerChange(e: ChangeEvent<HTMLInputElement>): void {
    this.setState({ answer: e.target.value });
  }

  /** Hides the block without sending anything. */
  @bind
  protected onDismiss(): void {
    this.setState({ dismissed: true });
  }

  /**
   * Renders one suggested-option chip.
   *
   * @param opt - the option text
   * @param i - the option index
   */
  @bind
  protected renderOption(opt: string, i: number) {
    return <OptionChip key={i} option={opt} onPick={this.answerWith} />;
  }

  /** Renders the question, its chips and the answer form. */
  public render() {
    const { question, options } = this.props;
    const { dismissed, answer } = this.state;
    if (dismissed) return null;

    return (
      <div data-test-id="ask-question-block" className="tool-block my-2 border-accent/40 bg-accent/[6%]">
        <div className="flex items-center justify-between gap-2 mb-2">
          <div className="flex items-center gap-2">
            <HelpCircle size={15} className="text-accent shrink-0" />
            <span className="text-accent font-semibold text-sm">Agent is waiting for your answer</span>
          </div>
          <button
            type="button"
            onClick={this.onDismiss}
            title="Dismiss — does not send anything"
            className="text-text-subtle hover:text-text transition-colors shrink-0"
          >
            <X size={15} />
          </button>
        </div>

        <p className="text-text text-sm leading-relaxed mb-3 whitespace-pre-wrap">{question}</p>

        {options.length > 0 && (
          <div data-test-id="ask-question-options" className="flex flex-wrap gap-2 mb-3">
            {options.map(this.renderOption)}
          </div>
        )}

        <form onSubmit={this.onSubmit} className="flex items-center gap-2">
          <input
            type="text"
            value={answer}
            onChange={this.onAnswerChange}
            placeholder="Type your answer…"
            data-test-id="ask-question-answer-input"
            className="flex-1 bg-base-subtle border border-surface rounded-lg px-3 py-2 text-sm text-text outline-none focus:border-accent"
          />
          <button
            type="submit"
            disabled={!answer.trim()}
            data-test-id="ask-question-answer-submit"
            className="px-3 py-2 text-sm btn-primary disabled:opacity-30"
          >
            Send
          </button>
        </form>
      </div>
    );
  }
}
