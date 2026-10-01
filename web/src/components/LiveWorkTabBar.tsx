// Tab selector for LiveWorkPanel (task #1059, stage 5b): Tasks / Commands N /
// Agents N / Schedules N. Tasks is always shown; the others hide while their
// list is empty. Also hosts StatusBar (moved out of TodoList's own header --
// it's now one tab's body among four, not the panel's own shell).

import { StatusBar } from "./StatusBar";

export type LiveWorkTabKey = "tasks" | "commands" | "agents" | "schedules";

function TabButton({
  active,
  label,
  onClick,
  testId,
}: {
  active: boolean;
  label: string;
  onClick: () => void;
  testId: string;
}) {
  return (
    <button
      type="button"
      data-test-id={testId}
      onClick={onClick}
      aria-pressed={active}
      className={`shrink-0 px-3 py-2 font-semibold uppercase tracking-wider transition-colors border-b-2 ${
        active
          ? "text-text border-accent"
          : "text-text-subtle hover:text-text border-transparent"
      }`}
      style={{ fontSize: "var(--chat-font-size)" }}
    >
      {label}
    </button>
  );
}

export function LiveWorkTabBar({
  tab,
  onSelect,
  commandCount,
  agentCount,
  scheduleCount,
}: {
  tab: LiveWorkTabKey;
  onSelect: (tab: LiveWorkTabKey) => void;
  commandCount: number;
  agentCount: number;
  scheduleCount: number;
}) {
  return (
    <div data-test-id="live-work-tabs" className="flex items-center border-t border-surface bg-base-subtle/40">
      <TabButton active={tab === "tasks"} label="Tasks" onClick={() => onSelect("tasks")} testId="live-work-tab-tasks" />
      {commandCount > 0 && (
        <TabButton
          active={tab === "commands"}
          label={`Commands ${commandCount}`}
          onClick={() => onSelect("commands")}
          testId="live-work-tab-commands"
        />
      )}
      {agentCount > 0 && (
        <TabButton
          active={tab === "agents"}
          label={`Agents ${agentCount}`}
          onClick={() => onSelect("agents")}
          testId="live-work-tab-agents"
        />
      )}
      {scheduleCount > 0 && (
        <TabButton
          active={tab === "schedules"}
          label={`Schedules ${scheduleCount}`}
          onClick={() => onSelect("schedules")}
          testId="live-work-tab-schedules"
        />
      )}
      <div className="flex-1 flex items-center justify-center min-w-0 px-3">
        <StatusBar inline />
      </div>
    </div>
  );
}
