// Schedules tab body for LiveWorkPanel (stage 5b): the active session's
// wake schedules from the server's durable wake_schedules snapshot
// (session_wake_schedules events). Each active row carries an explicit
// Cancel button gated by the shared ConfirmDialog; the server's fresh
// snapshot reply is what actually removes the row.

import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { WakeScheduleItem } from "../types";
import { sendCancelWakeSchedule, pickPendingWakeSchedule } from "../store";
import { wakeCancelCarburetor } from "../state/wakeCancel";
import { ConfirmDialog } from "./ConfirmDialog";

/**
 * Formats a unix-ms timestamp in the browser's locale.
 *
 * @param ms - unix milliseconds
 */
function formatLocalTime(ms: number): string {
  return new Date(ms).toLocaleString();
}

/**
 * Describes how often a schedule fires.
 *
 * @param item - the schedule row
 */
function recurrenceLabel(item: WakeScheduleItem): string {
  if (item.kind !== "loop") return "one-time";
  const total = item.maxRuns > 0 ? String(item.maxRuns) : "∞";
  const parts = [`every ${Math.round(item.everyMs / 1000)}s`, `${item.occurrence}/${total}`];
  if (item.untilAt > 0) parts.push(`until ${formatLocalTime(item.untilAt)}`);
  return parts.join(" · ");
}

/** Props of one schedule row. */
interface IWakeScheduleRowProps {
  item: WakeScheduleItem;
  onCancelRequest: (item: WakeScheduleItem) => void;
}

/** One wake-schedule row with its Cancel button. */
class WakeScheduleRow extends AntiHookComponent<IWakeScheduleRowProps> {
  /** Asks the list to open the cancel confirmation for this row. */
  @bind
  protected requestCancel(): void {
    this.props.onCancelRequest(this.props.item);
  }

  /** Renders the row. */
  public render() {
    const { item } = this.props;
    const active = item.state === "active";
    return (
      <div data-test-id="wake-schedule-row" className="flex items-center gap-2 px-2 py-1.5 rounded hover:bg-base-overlay/50 transition-colors">
        <span data-test-id="wake-schedule-kind" className="text-mauve font-semibold text-sm shrink-0">
          {item.kind === "loop" ? "loop" : "wake"}
        </span>
        <span className="text-text font-mono text-sm truncate flex-1 min-w-0" title={item.message} style={{ fontSize: "var(--chat-font-size)" }}>
          {item.message || "—"}
        </span>
        <span data-test-id="wake-schedule-detail" className="text-text-subtle text-xs font-mono shrink-0">
          {recurrenceLabel(item)}
        </span>
        {item.state === "active" ? (
          <span className="text-text-subtle text-xs font-mono tabular-nums shrink-0">
            {formatLocalTime(item.nextRunAt)}
          </span>
        ) : (
          <span data-test-id="wake-schedule-state" className="text-text-subtle text-xs font-mono shrink-0">
            {item.state}
          </span>
        )}
        {active && (
          <button
            type="button"
            data-test-id="wake-schedule-cancel"
            onClick={this.requestCancel}
            className="shrink-0 px-2 py-0.5 text-xs rounded text-red hover:bg-red/10 transition-colors"
          >
            Cancel
          </button>
        )}
      </div>
    );
  }
}

/**
 * Builds the confirmation text for cancelling a schedule.
 *
 * @param item - the schedule about to be cancelled
 */
function cancelMessage(item: WakeScheduleItem): string {
  const future = item.kind === "loop" ? " (all future occurrences)" : "";
  return `Stop "${item.message || item.id}" from waking the agent${future}?`;
}

/** Props of the schedules list. */
interface IWakeScheduleListProps {
  items: WakeScheduleItem[];
  sessionID: string;
  emptyLabel: string;
}

/** Local state of the schedules list. */
interface IWakeScheduleListState {
  pending: WakeScheduleItem | null;
}

/** Schedules tab body: the session's wake schedules with a confirmed Cancel flow. */
export class WakeScheduleList extends AntiHookComponent<IWakeScheduleListProps, IWakeScheduleListState> {
  public state: IWakeScheduleListState = { pending: null };

  /**
   * Opens the cancel confirmation for a row.
   *
   * @param item - the schedule the operator wants to cancel
   */
  @bind
  protected requestCancel(item: WakeScheduleItem): void {
    wakeCancelCarburetor.setError(null);
    this.setState({ pending: item });
  }

  /** Closes the confirmation and forgets any inline error. */
  @bind
  protected closeCancel(): void {
    wakeCancelCarburetor.setError(null);
    this.setState({ pending: null });
  }

  // Confirm sends and KEEPS the dialog open: the server's fresh snapshot is
  // what removes the row, while a rejected cancel lands as an inline error
  // via the dialog's error prop. Success detection is the row's
  // disappearance from the snapshot -- the cancel reply carries no request
  // correlation beyond its WS id.
  /** Sends the cancel request for the still-listed pending schedule. */
  @bind
  protected confirmCancel(): void {
    const { items, sessionID } = this.props;
    const effectivePending = pickPendingWakeSchedule(items, this.state.pending);
    if (!effectivePending) return;
    sendCancelWakeSchedule(sessionID, effectivePending.id);
  }

  /**
   * Renders one schedule row.
   *
   * @param item - the schedule to show
   */
  @bind
  protected renderRow(item: WakeScheduleItem) {
    return <WakeScheduleRow key={item.id} item={item} onCancelRequest={this.requestCancel} />;
  }

  /** Renders the rows and, while a cancel is pending, the confirmation. */
  public render() {
    const { items, emptyLabel } = this.props;
    const { error: cancelError } = this.useCarburetor(wakeCancelCarburetor);
    // Derived during render, deliberately NOT a synchronizing effect: the
    // dialog shows only while the pending row still exists, so the snapshot
    // that removes it closes the dialog with no extra state.
    const effectivePending = pickPendingWakeSchedule(items, this.state.pending);

    return (
      <div data-test-id="wake-schedule-list" className="px-2 py-2">
        {items.length === 0 ? (
          <p className="text-text-muted px-2 py-1" style={{ fontSize: "var(--chat-font-size)" }}>{emptyLabel}</p>
        ) : (
          items.map(this.renderRow)
        )}
        {effectivePending && (
          <ConfirmDialog
            title="Cancel wake schedule"
            message={cancelMessage(effectivePending)}
            confirmLabel="Cancel schedule"
            error={cancelError}
            onConfirm={this.confirmCancel}
            onCancel={this.closeCancel}
          />
        )}
      </div>
    );
  }
}
