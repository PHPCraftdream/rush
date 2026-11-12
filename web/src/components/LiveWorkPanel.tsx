import { useState } from "react";
import type { TReadonly } from "@react-carburetor/core";
import { useCarburetorValue } from "@react-carburetor/core/interop";
import type { Todo, WakeScheduleItem } from "../types";
import { pickBatchWork } from "../batchWork";
import type { BatchWorkState } from "../batchWork";
import { pickWakeSchedules } from "../store";
import { batchWorkCarburetor } from "../state/batchWork";
import type { IBatchWorkState } from "../state/BatchWorkCarburetor";
import { wakeSchedulesCarburetor } from "../state/wakeSchedules";
import type { IWakeSchedulesState } from "../state/WakeSchedulesCarburetor";
import { TodoList } from "./TodoList";
import { LiveWorkTabBar, type LiveWorkTabKey } from "./LiveWorkTabBar";
import { WakeScheduleList } from "./WakeScheduleList";
import { BatchTree } from "./BatchTree";

/** Picks the per-session batch table out of the batch-work state. */
const selectBatchBySession = (state: TReadonly<IBatchWorkState>) => new Map(Object.entries(state.bySession));

/** Picks the per-session schedule table out of the wake-schedules state. */
const selectSchedulesBySession = (state: TReadonly<IWakeSchedulesState>) => new Map(Object.entries(state.bySession));

export function LiveWorkPanel({ sessionID, todos }: { sessionID: string; todos: Todo[] }) {
  const batchWorkMap = useCarburetorValue(batchWorkCarburetor, selectBatchBySession) as ReadonlyMap<string, BatchWorkState>;
  const snapshot = pickBatchWork(batchWorkMap, sessionID)?.lastGood;
  const schedulesMap = useCarburetorValue(wakeSchedulesCarburetor, selectSchedulesBySession) as ReadonlyMap<string, WakeScheduleItem[]>;
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
