// Auto-sizing textarea used for editing user messages, thinking parts and
// summaries. Owns all editing state and handlers.

import { createRef } from "react";
import type { ChangeEvent, KeyboardEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";

/** Props of the edit form. */
interface IEditFormProps {
  initialValue: string;
  rows: number;
  className: string;
  onSave: (text: string) => void;
  onCancel: () => void;
}

/** Local state of the edit form. */
interface IEditFormState {
  value: string;
}

/** The textarea grows with its content instead of scrolling. */
const TEXTAREA_STYLE = { overflow: "hidden" } as const;

/** Textarea with Save/Cancel; Ctrl/Cmd+Enter saves and Escape cancels. */
export class EditForm extends AntiHookComponent<IEditFormProps, IEditFormState> {
  public state: IEditFormState = { value: this.props.initialValue };

  private readonly taRef = createRef<HTMLTextAreaElement>();

  /** Focuses and sizes the textarea once, after mount. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect(this.focusAndSize, []);
  }

  /** Puts the caret at the end and fits the height to the content. */
  @bind
  protected focusAndSize(): void {
    const ta = this.taRef.current;
    if (!ta) return;
    ta.focus();
    ta.selectionStart = ta.value.length;
    ta.style.height = "auto";
    ta.style.height = ta.scrollHeight + "px";
  }

  /**
   * Tracks the typed value and re-fits the height.
   *
   * @param e - the textarea change event
   */
  @bind
  protected onInput(e: ChangeEvent<HTMLTextAreaElement>): void {
    this.setState({ value: e.target.value });
    e.target.style.height = "auto";
    e.target.style.height = e.target.scrollHeight + "px";
  }

  /** Saves the current value. */
  @bind
  protected commit(): void {
    this.props.onSave(this.state.value);
  }

  /**
   * Escape cancels; Ctrl/Cmd+Enter saves.
   *
   * @param e - the keydown event
   */
  @bind
  protected onKey(e: KeyboardEvent<HTMLTextAreaElement>): void {
    if (e.key === "Escape") this.props.onCancel();
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) this.props.onSave(this.state.value);
  }

  /** Renders the textarea and its buttons. */
  public render() {
    const { rows, className, onCancel } = this.props;
    return (
      <div className="flex flex-col gap-2">
        <textarea
          ref={this.taRef}
          value={this.state.value}
          onChange={this.onInput}
          onKeyDown={this.onKey}
          rows={rows}
          className={className}
          style={TEXTAREA_STYLE}
        />
        <div className="flex gap-2">
          <button onClick={onCancel} className="btn-ghost-sm">Cancel</button>
          <button onClick={this.commit} className="px-3 py-1 text-xs btn-primary">Save</button>
        </div>
      </div>
    );
  }
}
