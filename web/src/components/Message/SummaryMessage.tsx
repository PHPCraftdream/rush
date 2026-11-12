// Collapsible context-condensation ("summary") message card.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { BookMarked, Pencil } from "lucide-react";
import ReactMarkdown from "react-markdown";
import type { Message as Msg } from "../../types";
import { updateMessageContent } from "../../store";
import { DurationBadge } from "./DurationBadge";
import { EditForm } from "./EditForm";
import { EffortBadge } from "./EffortBadge";
import { MD_REMARK } from "./mdRemarkPlugins";
import { MD_REHYPE } from "./mdRehypePlugins";
import { watchCollapseAllSignal } from "./watchCollapseAllSignal";
import { extractText } from "./extractText";
import { isTerminallyFinished } from "./isTerminallyFinished";

/** Props of the summary card. */
interface ISummaryMessageProps {
  message: TReadonly<Msg>;
}

/** Local state of the summary card. */
interface ISummaryMessageState {
  editing: boolean;
  open: boolean;
}

/** Context-condensation card: collapsed spoiler with an optional edit form. */
export class SummaryMessage extends AntiHookComponent<ISummaryMessageProps, ISummaryMessageState> {
  public state: ISummaryMessageState = { editing: false, open: false };

  /** Subscribes to the global "Collapse all" signal. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.watchCollapseAll, []);
  }

  /** Starts watching the collapse-all signal; returns its disposer. */
  @bind
  protected watchCollapseAll(): () => void {
    return watchCollapseAllSignal(this.collapse);
  }

  /** Folds the summary body. */
  @bind
  protected collapse(): void {
    this.setState({ open: false });
  }

  /** Flips the summary body. */
  @bind
  protected toggleOpen(): void {
    this.setState({ open: !this.state.open });
  }

  /** Flips edit mode. */
  @bind
  protected toggleEditing(): void {
    this.setState({ editing: !this.state.editing });
  }

  /** Leaves edit mode without saving. */
  @bind
  protected cancelEditing(): void {
    this.setState({ editing: false });
  }

  /**
   * Persists a changed summary text and leaves edit mode.
   *
   * @param newText - the edited text
   */
  @bind
  protected handleSave(newText: string): void {
    const { message } = this.props;
    if (newText && newText !== extractText(message.Parts)) updateMessageContent(message.ID, newText);
    this.setState({ editing: false });
  }

  /** Renders the card header and the summary body. */
  public render() {
    const { message } = this.props;
    const { editing, open } = this.state;
    const text = extractText(message.Parts);
    const isFinished = isTerminallyFinished(message.Parts);

    return (
      <div className="px-8 py-3">
        <div className="summary-card">
          <div className="summary-header">
            <BookMarked size={15} className="text-yellow shrink-0" />
            <span className="text-sm font-semibold text-yellow">Context condensed</span>
            <span className="ml-auto text-xs text-text-muted font-mono flex items-center gap-1">
              {message.Model}
              <EffortBadge effort={message.ReasoningEffort} />
            </span>
            {isFinished && <DurationBadge message={message} />}
            {isFinished && (
              <button onClick={this.toggleEditing} title="Edit summary" className="btn-icon-sm ml-1">
                <Pencil size={13} />
              </button>
            )}
          </div>
          <div className="group">
            <button
              type="button"
              onClick={this.toggleOpen}
              aria-expanded={open}
              className="summary-toggle w-full text-left bg-transparent border-0"
            >
              {open ? "Hide summary ▾" : "Show summary ▸"}
            </button>
            {open && (editing ? (
              <div className="summary-body chat-font">
                <EditForm
                  initialValue={text}
                  rows={4}
                  className="field-textarea"
                  onSave={this.handleSave}
                  onCancel={this.cancelEditing}
                />
              </div>
            ) : text ? (
              <div className="summary-body md">
                <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
              </div>
            ) : null)}
          </div>
        </div>
      </div>
    );
  }
}
