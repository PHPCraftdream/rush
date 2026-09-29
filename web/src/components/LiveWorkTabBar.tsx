// Tab selector for LiveWorkPanel (task #1059): Tasks / Commands N / Agents N.
// Tasks is always shown; Commands/Agents hide while their list is empty.
// Also hosts StatusBar (moved out of TodoList's own header -- it's now one
// tab's body among three, not the panel's own shell).

import { StatusBar } from "./StatusBar";

export type LiveWorkTabKey = "tasks" | "commands" | "agents";

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
}: {
  tab: LiveWorkTabKey;
  onSelect: (tab: LiveWorkTabKey) => void;
  commandCount: number;
  agentCount: number;
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
      <div className="flex-1 flex items-center justify-center min-w-0 px-3">
        <StatusBar inline />
      </div>
    </div>
  );
}
