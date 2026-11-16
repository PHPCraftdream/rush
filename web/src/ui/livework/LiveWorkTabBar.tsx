import { AntiHookComponent, bind } from "@react-carburetor/core";
import { StatusBar } from "./StatusBar";

/** Tabs of the live-work panel. */
export type LiveWorkTabKey = "tasks" | "batches" | "schedules";

/** Props of one tab button. */
interface ITabButtonProps {
  active: boolean;
  label: string;
  tab: LiveWorkTabKey;
  onSelect: (tab: LiveWorkTabKey) => void;
  testId: string;
}

/** One tab button of the live-work tab bar. */
class TabButton extends AntiHookComponent<ITabButtonProps> {
  /** Reports this button's tab as selected. */
  @bind
  protected select(): void {
    this.props.onSelect(this.props.tab);
  }

  /** Renders the button. */
  public render() {
    const { active, label, testId } = this.props;
    const tone = active ? "text-text border-accent" : "text-text-subtle hover:text-text border-transparent";
    return (
      <button
        type="button"
        data-test-id={testId}
        onClick={this.select}
        aria-pressed={active}
        className={`shrink-0 px-3 py-2 font-semibold uppercase tracking-wider transition-colors border-b-2 ${tone}`}
        style={{ fontSize: "var(--chat-font-size)" }}
      >
        {label}
      </button>
    );
  }
}

/** Props of the tab bar. */
interface ILiveWorkTabBarProps {
  tab: LiveWorkTabKey;
  onSelect: (tab: LiveWorkTabKey) => void;
  batchCount: number;
  scheduleCount: number;
}

/** Tab strip of the live-work panel, with the inline status widget on the right. */
export class LiveWorkTabBar extends AntiHookComponent<ILiveWorkTabBarProps> {
  /** Renders the tabs; Batches/Schedules appear only when they have entries. */
  public render() {
    const { tab, onSelect, batchCount, scheduleCount } = this.props;
    return (
      <div data-test-id="live-work-tabs" className="flex items-center border-t border-surface bg-base-subtle/40">
        <TabButton
          active={tab === "tasks"}
          label="Tasks"
          tab="tasks"
          onSelect={onSelect}
          testId="live-work-tab-tasks"
        />
        {batchCount > 0 && (
          <TabButton
            active={tab === "batches"}
            label={`Batches ${batchCount}`}
            tab="batches"
            onSelect={onSelect}
            testId="live-work-tab-batches"
          />
        )}
        {scheduleCount > 0 && (
          <TabButton
            active={tab === "schedules"}
            label={`Schedules ${scheduleCount}`}
            tab="schedules"
            onSelect={onSelect}
            testId="live-work-tab-schedules"
          />
        )}
        <div className="flex-1 flex items-center justify-center min-w-0 px-3"><StatusBar inline /></div>
      </div>
    );
  }
}
