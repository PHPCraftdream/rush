import { useState } from "react";
import { useStore } from "@nanostores/react";
import type { Todo } from "../types";
import { $batchWorkBySession, pickBatchWork } from "../batchWork";
import { $wakeSchedulesBySession, pickWakeSchedules } from "../store_livework";
import { TodoList } from "./TodoList";
import { LiveWorkTabBar, type LiveWorkTabKey } from "./LiveWorkTabBar";
import { WakeScheduleList } from "./WakeScheduleList";
import { BatchTree } from "./BatchTree";

export function LiveWorkPanel({ sessionID, todos }: { sessionID: string; todos: Todo[] }) {
  const batchWorkMap = useStore($batchWorkBySession);
  const snapshot = pickBatchWork(batchWorkMap, sessionID)?.lastGood;
  const schedulesMap = useStore($wakeSchedulesBySession);
  const schedules = pickWakeSchedules(schedulesMap, sessionID);
  const [tab, setTab] = useState<LiveWorkTabKey>("tasks");
  const effectiveTab = tab === "schedules" && schedules.length === 0 ? "tasks" : tab;

  return (
    <div data-test-id="live-work-panel" className="shrink-0">
      <LiveWorkTabBar tab={effectiveTab} onSelect={setTab} batchCount={snapshot?.roots.length ?? 0} scheduleCount={schedules.length} />
      {effectiveTab === "tasks" && <TodoList sessionID={sessionID} todos={todos} />}
      {effectiveTab === "batches" && <BatchTree sessionID={sessionID} />}
      {effectiveTab === "schedules" && <WakeScheduleList items={schedules} sessionID={sessionID} emptyLabel="No wake schedules." />}
    </div>
  );
}
