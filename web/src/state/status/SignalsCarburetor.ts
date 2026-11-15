import { Carburetor } from "@react-carburetor/core";

/**
 * A request to force one tool call open: one target id per request, with a
 * nonce so repeating the same target still counts as a new request.
 */
export interface ExpandToolCallRequest {
  toolCallID: string;
  nonce: number;
}

/** One-way UI signals that collapsible transcript components react to. */
export interface ISignalsState {
  /**
   * Incremented whenever the operator clicks "Collapse all spoilers". Collapsible
   * components close their local open-state on each tick, skipping the initial
   * render so mounting alone collapses nothing.
   */
  collapseAllNonce: number;
  /** Latest "expand and jump to this tool call" request; the panel scrolls the anchor into view. */
  expandToolCallRequest: ExpandToolCallRequest | null;
}

/**
 * Bumps the collapse-all counter in the draft.
 *
 * @param draft - state to change
 */
const writeCollapse = (draft: ISignalsState): void => {
  draft.collapseAllNonce += 1;
};

/**
 * Writes an expand request into the draft.
 *
 * @param draft - state to change
 * @param request - the request
 */
const writeExpand = (draft: ISignalsState, request: ExpandToolCallRequest): void => {
  draft.expandToolCallRequest = request;
};

/** Holds the collapse-all and expand-tool-call signals. */
export class SignalsCarburetor extends Carburetor<ISignalsState> {
  private expandNonce = 0;

  /** Signals every collapsible component to close. */
  public collapseAll(): void {
    this.update(writeCollapse);
  }

  /**
   * Asks the matching tool call to open; transcript-level rows call it on item click.
   *
   * @param toolCallID - tool call to expand
   */
  public requestExpandToolCall(toolCallID: string): void {
    this.expandNonce += 1;
    this.update(writeExpand, { toolCallID, nonce: this.expandNonce });
  }
}
