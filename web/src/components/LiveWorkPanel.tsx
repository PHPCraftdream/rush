import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { Todo, WakeScheduleItem } from "../types";
import { batchWorkCarburetor } from "../state/batchWork";
import { wakeSchedulesCarburetor } from "../state/wakeSchedules";
import { TodoList } from "./TodoList";
import { LiveWorkTabBar, type LiveWorkTabKey } from "./LiveWorkTabBar";
import { WakeScheduleList } from "./WakeScheduleList";
import { BatchTree } from "./BatchTree";

/** Shared empty list for a session without schedules. */
const EMPTY_SCHEDULES: TReadonly<WakeScheduleItem[]> = [];

/** Props of the live-work panel. */
interface ILiveWorkPanelProps {
  sessionID: string;
  todos: Todo[];
}

/** Local state of the live-work panel. */
interface ILiveWorkPanelState {
  tab: LiveWorkTabKey;
}

/** Tabbed panel above the chat input: tasks, batches and wake schedules of the active session. */
export class LiveWorkPanel extends AntiHookComponent<ILiveWorkPanelProps, ILiveWorkPanelState> {
  public state: ILiveWorkPanelState = { tab: "tasks" };

  /** Detached snapshot of the session's schedules, safe to hand to a child. */
  private readonly schedulesView = this.connectSelection(
    wakeSchedulesCarburetor,
    (data) => data.bySession[this.props.sessionID] ?? EMPTY_SCHEDULES,
  );

  /**
   * Switches the visible tab.
   *
   * @param tab - the tab to show
   */
  @bind
  protected selectTab(tab: LiveWorkTabKey): void {
    this.setState({ tab });
  }

  /** Renders the tab bar and the selected tab body. */
  public render() {
    const { sessionID, todos } = this.props;
    const { bySession } = this.useCarburetor(batchWorkCarburetor);
    const batchCount = bySession[sessionID]?.lastGood?.roots.length ?? 0;
    const schedules = this.schedulesView();
    const { tab } = this.state;
    const effectiveTab = tab === "schedules" && schedules.length === 0 ? "tasks" : tab;

    return (
      <div data-test-id="live-work-panel" className="shrink-0">
        <LiveWorkTabBar
          tab={effectiveTab}
          onSelect={this.selectTab}
          batchCount={batchCount}
          scheduleCount={schedules.length}
        />
        {effectiveTab === "tasks" && <TodoList sessionID={sessionID} todos={todos} />}
        {effectiveTab === "batches" && <BatchTree sessionID={sessionID} />}
        {effectiveTab === "schedules" && (
          <WakeScheduleList
            items={schedules as WakeScheduleItem[]}
            sessionID={sessionID}
            emptyLabel="No wake schedules."
          />
        )}
      </div>
    );
  }
}
