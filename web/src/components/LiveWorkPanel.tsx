// Tabbed panel shown at the bottom of the chat (task #1059): Tasks (the
// existing todo list, always present) / Commands / Agents (in-flight async
// jobs and sub-agent delegations, hidden while empty). Replaces the bare
// <TodoList> that used to sit in this spot in Chat.tsx.
//
// The server-side emitter that actually populates commands/agents lands in
// #1058 (needs the DB-backed readers) -- until then $liveWorkBySession stays
// empty for every session, so only the Tasks tab is ever visible in
// practice. The tab machinery, the anchor/expand wiring, and the wire
// contract are all real and exercised by web/tests/live-work-panel.spec.ts
// against the mock-WS fixtures.

import { useState } from "react";
import { useStore } from "@nanostores/react";
import type { Todo } from "../types";
import { TodoList } from "./TodoList";
import { LiveWorkTabBar, type LiveWorkTabKey } from "./LiveWorkTabBar";
import { LiveWorkList } from "./LiveWorkList";
import { $liveWorkBySession, pickLiveWork } from "../store_livework";

export function LiveWorkPanel({ sessionID, todos }: { sessionID: string; todos: Todo[] }) {
  // Read via useStore's OWN return value, not a side-channel $liveWorkBySession.get()
  // call -- see pickLiveWork's doc comment: the React Compiler can't see through
  // the latter and will memoize stale JSX across atom updates it doesn't track.
  const liveWorkMap = useStore($liveWorkBySession);
  const work = pickLiveWork(liveWorkMap, sessionID);
  const hasCommands = work.commands.length > 0;
  const hasAgents = work.agents.length > 0;

  const [tab, setTab] = useState<LiveWorkTabKey>("tasks");
  // Fall back to Tasks the instant the selected tab's list empties -- Tasks
  // is the only tab guaranteed to always be present. Derived during render
  // (not a synchronizing effect): `tab` is only ever written directly by
  // onSelect below, so there's nothing external to catch up with here.
  const effectiveTab: LiveWorkTabKey =
    (tab === "commands" && !hasCommands) || (tab === "agents" && !hasAgents) ? "tasks" : tab;

  return (
    <div data-test-id="live-work-panel" className="shrink-0">
      <LiveWorkTabBar tab={effectiveTab} onSelect={setTab} commandCount={work.commands.length} agentCount={work.agents.length} />
      {effectiveTab === "tasks" && <TodoList sessionID={sessionID} todos={todos} />}
      {effectiveTab === "commands" && <LiveWorkList items={work.commands} emptyLabel="No commands running." />}
      {effectiveTab === "agents" && <LiveWorkList items={work.agents} emptyLabel="No agents running." />}
    </div>
  );
}
