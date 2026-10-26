import { StatusBar } from "./StatusBar";
export type LiveWorkTabKey = "tasks" | "batches" | "schedules";

function TabButton({ active, label, onClick, testId }: { active: boolean; label: string; onClick: () => void; testId: string }) {
  return <button type="button" data-test-id={testId} onClick={onClick} aria-pressed={active} className={`shrink-0 px-3 py-2 font-semibold uppercase tracking-wider transition-colors border-b-2 ${active ? "text-text border-accent" : "text-text-subtle hover:text-text border-transparent"}`} style={{ fontSize: "var(--chat-font-size)" }}>{label}</button>;
}

export function LiveWorkTabBar({ tab, onSelect, batchCount, scheduleCount }: { tab: LiveWorkTabKey; onSelect: (tab: LiveWorkTabKey) => void; batchCount: number; scheduleCount: number }) {
  return <div data-test-id="live-work-tabs" className="flex items-center border-t border-surface bg-base-subtle/40">
    <TabButton active={tab === "tasks"} label="Tasks" onClick={() => onSelect("tasks")} testId="live-work-tab-tasks" />
    {batchCount > 0 && <TabButton active={tab === "batches"} label={`Batches ${batchCount}`} onClick={() => onSelect("batches")} testId="live-work-tab-batches" />}
    {scheduleCount > 0 && <TabButton active={tab === "schedules"} label={`Schedules ${scheduleCount}`} onClick={() => onSelect("schedules")} testId="live-work-tab-schedules" />}
    <div className="flex-1 flex items-center justify-center min-w-0 px-3"><StatusBar inline /></div>
  </div>;
}
