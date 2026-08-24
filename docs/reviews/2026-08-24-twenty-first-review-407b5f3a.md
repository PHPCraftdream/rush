# Twenty-first review — `9c1d661b..407b5f3a`

**Scope, as instructed:**

1. Independent verification of `407b5f3a` (twentieth review's M-1 + M-2), including a
   stress test of the new AND-of-two-signals `done` predicate for cases the worker
   did not consider, and confirmation of the two documented residuals.
2. A fresh end-to-end re-read of `web/src/components/SubAgentBlock.tsx`, not filtered
   through M-1/M-2's shape.
3. A decisive stance on the two items previous rounds deferred: the never-unsubscribed
   one-shot `ws.on` modal handlers, and the `TodoList` optimistic-edit race.

**Out of scope by instruction, and honoured:** `handlers_agent.go`'s
`handleRerunMessage` / `recreateRerunPromptIfLost` (not opened); a general backend
sweep (backend read only where a `web/` question required knowing what the server
puts on the wire — `handlers_sessions.go`, `handlers_messages.go`, `handlers_mcp.go`,
`hub.go`, `events.go`, `agent_turn.go`, `agent_compaction.go`, `session_read.go`,
`db/sql/sessions.sql`, and fantasy v0.25.2's `agent.go`).

**Date:** 2026-08-24
**Reviewed at:** `407b5f3a` (working tree: the known ` D web/dist/.gitkeep` plus
untracked `docs/`; nothing else. This review changed nothing in the repo — the
harness lives in the OS temp dir).

---

## Verdict

**GO on shipping `407b5f3a`. NO-GO on "nothing to file."**

`407b5f3a` is a strict improvement and nothing in it needs to be reverted. Every claim
in its commit message about the *old* code is true, and the new predicate is correct on
every case the worker enumerated — I re-derived all of it by execution, not by reading.

But the round does not come back empty, and the reason is uncomfortable: **the fix's
central design argument is false.** `407b5f3a` chose `!isRunning && isTerminallyFinished(last)`
over the simpler `!isRunning` because "busySessions lags up to 5s for sub-agents". It
does not lag. `$busySessions` **never** contains a sub-agent session ID at all — not after
5 s, not ever — because `list_sessions` is backed by SQL that filters children out. So the
`!isRunning` conjunct is a constant `true`, the AND is a no-op, and the step-boundary
window the commit believed it had closed is still open at every step boundary of every
sub-agent run. Three other affordances in the same component that key off `isRunning`
are dead code for the same reason, and the explanatory comment the commit added
(`SubAgentBlock.tsx:77-80`) asserts a mechanism that does not exist.

Pulling that thread found a second, larger defect in the same component: the lazy
`load_messages` the block fires on mount — added specifically so that "past runs surfaced
after a reload" would populate — has its reply **silently discarded** by `useWS`'s
`messages_list` router, for the same root reason. In practice that means every historical
sub-agent block is empty after a page reload.

And both deferred items turned out to be real. Neither is inert. I have concrete,
executed reproductions for both.

**Findings: 2 Major (P2), 3 Minor.** No P1. No data at risk on disk; the two Minor
data-loss items are in-flight operator input (a typed MCP config, a typed todo).

---

## §0 — Harness

Everything below marked *(executed)* comes from one Node 24 harness living in the OS
temp dir (`%TEMP%/crush-r21/`), outside the repository. It imports the **real**
`web/src/store.ts`, `web/src/ws.ts` and `web/src/components/Message/textParts.ts` by
absolute file URL (their own bare specifiers resolve against `web/node_modules`), with
`WebSocket` / `location` / `localStorage` stubbed before load and a `module.registerHooks`
resolver for the extensionless relative specifiers the bundler normally handles. Component
logic under test is copied **verbatim** from the sources, line ranges cited at each site.
Final tally:

```
31 passed, 2 failed
```

The two failures are the demonstration, not harness bugs: they are the two timeline
points where the shipped `done` predicate reports a still-running sub-agent as finished.

---

## §1 — Independent verification of `407b5f3a`

### 1.1 The old predicate really was broken, in both ways claimed

Confirmed against current backend source rather than the commit message:

- `OnStepFinish` (`agent_turn.go:1295`) reaches `currentAssistant.AddFinish(finishReason, "", "")`
  at `:1380` on **every** step, with `finishReason = message.FinishReasonToolUse` for a step
  that ended in tool calls (`:1315-1322`). `Message.AddFinish` (`message/content.go:501-510`)
  appends `Finish{…}` with `Partial` unset — a genuine non-Partial terminal-looking finish.
- `PrepareStep` (`agent_turn.go:990`) calls `a.messages.Create(…Role: message.Assistant…)`
  at `:1113-1123`, i.e. the next step's message is a brand-new row. So step 1's finished
  message does sit in the store next to step 2's live one.
- The 2 s checkpoint ticker stamps `Partial: true` (`agent_turn.go:799-805`), which
  `PartWire.Partial` carries (`wire.go:32-38,116`, `omitempty`) and the old `isFinished`
  ignored.

So `messages.some(m => m.Role === "assistant" && hasAnyFinish(m))` was true from the end of
step 1 onward. The commit's diagnosis is exact.

### 1.2 The new predicate is correct wherever it can be

*(executed)* — verbatim copy of `SubAgentBlock.tsx:93-97` against the real
`isTerminallyFinished`, over a wire-shaped two-step sub-agent timeline:

```
t0  session created, nothing loaded yet            -> done=false   PASS
t1  user prompt only                               -> done=false   PASS
t2  step-1 assistant created, empty                -> done=false   PASS
t3  step-1 streaming text                          -> done=false   PASS
t4  step-1 + 2s auto-checkpoint (Partial finish)   -> done=false   PASS   <- old code said true
t5  step-1 OnStepFinish (non-Partial tool_use)     -> done=true    FAIL   <- still running
t6  tool-result message appended (role=tool)       -> done=true    FAIL   <- still running
t7  step-2 assistant created by PrepareStep        -> done=false   PASS
t8  step-2 terminal finish (end_turn)              -> done=true    PASS
```

Residual-hunt cases, all correct:

```
halted-by-tool-result (tool_use rewritten to end_turn)   -> done      PASS
error finish (empty stream / peak-hours abort)           -> done      PASS
hard-killed run, no finish ever written                  -> not done  PASS
hidden compaction summary as last assistant              -> done      PASS
16 ms-batch reorder: message_created before finish flush -> not done  PASS
```

The last one matters and the worker did not enumerate it: `events.go` broadcasts
`message_created` **immediately** but batches `message_updated` on a 16 ms ticker
(`events.go:17,57-90`). So at a step boundary the client can receive step 2's *creation*
before step 1's *finish*, in which case `done` never rises at all. The predicate is correct
in that ordering too.

`t5`/`t6` are the residual the commit documents. What the commit gets wrong is how often
they are reachable — see F-1.

### 1.3 The other two sites in the commit

- `SummaryMessage.tsx:20` → `isTerminallyFinished(message.Parts)`. Correct and strictly
  safer: it gates the Edit pencil, and `updateMessageAndVerify` (`handlers_messages.go:70-78`)
  refuses writes to a still-streaming assistant message anyway, so the old version merely
  offered an affordance the server would reject.
- `DurationBadge.tsx:18` → same helper. Strictly better, as claimed: the timer now ticks
  at its own 100 ms cadence instead of freezing and resuming on every 2 s checkpoint.
- `AssistantContent.tsx:37` deliberately left alone. Correct call — I read it: its `isFinished`
  only ever runs under `!hasVisibleContent`, and the operative guard is `isLive`
  (`:61-62`). Changing it would alter the crash-window rendering, which is outside the
  task.

### 1.4 M-2's comment fix

`SubAgentBlock.tsx:109-113` now says the latch cannot distinguish "asked for THIS session"
from "asked for SOME session" and is "correct independently of how callers key us." Both
statements are true; the false `a-${idx}` premise is gone. `ToolActivityGroup.tsx:197` is
`key={`a-${part.ID}`}` at HEAD, and `Message/Part.tsx:26` renders a single unkeyed element.
Nothing to file.

### 1.5 Residuals — the two documented, plus a third

The commit documents two: (a) a step boundary can still flash done during the busy-lag
window; (b) a hard-killed run stays `done=false` until `agent_recovery.go` re-stamps
orphans at the next startup. Both confirmed. (a) is **understated by a lot** — see F-1.

**A third residual, not documented:** a sub-agent long enough to trip the sliding-window
compaction re-opens after it is already done. `agent_compaction.go:562-570` creates the
summary message with `Role: Assistant` and **empty parts**, then streams into it and only
adds the finish at `:628`. While that summary is streaming it is the *last assistant
message* with no terminal finish, so `done` flips **true → false** for the duration of the
summarisation call (seconds), the block re-expands, and the green badge disappears —
then it all flips back. Benign, cosmetic, and much rarer than F-1; recording it so the
residual list is complete rather than filing it.

---

## §2 — Findings

### F-1 (Major / P2) — `SubAgentBlock`'s `isRunning` is a constant `false`; the AND-guard, the "running…" badge, the Bot pulse and the "Starting agent…" placeholder are all dead

**Files:** `web/src/components/SubAgentBlock.tsx:72,77-80,93-97,141,143,149`
(root cause in `internal/db/sql/sessions.sql:84-88` + `internal/server/handlers_sessions.go:210-233`).

`isRunning = busySessions.has(subSessionID)` (`:72`). A sub-agent session ID never enters
`$busySessions`. The complete set of writers is one line — `setSessionBusy` at
`store.ts:323`, called only from `useWS.ts:289`'s `agent_busy` handler — and every
`EventAgentBusy` emitter in the backend is accounted for:

| emitter | SessionID it names |
|---|---|
| `handlers_agent.go:253,266` (`send_message`) | `p.SessionID` — client-chosen |
| `handlers_agent.go:419` (`cancel_agent`) | `p.SessionID` — client-chosen |
| `handlers_agent.go:444,456` (`summarize_session`) | `p.SessionID` — client-chosen |
| `handlers_agent.go:524,528` (`initialize`) | `sess.ID` — a freshly created **top-level** session |
| `handlers_agent.go:982,999` (rerun) | `sessionID` — client-chosen |
| `handlers_sessions.go:230` (the `list_sessions` correction loop) | `for s := range a.Sessions.List(ctx)` |

The client can only name a session it can select, and the sidebar filters
`ParentSessionID` (`Sidebar.tsx:15`). That leaves the `list_sessions` loop as the only
candidate — and it iterates `a.Sessions.List` → `session_read.go:108` →
`db.ListSessions`, whose SQL is:

```sql
-- internal/db/sql/sessions.sql:84-88   (and db/sessions.sql.go:362-366)
-- name: ListSessions :many
SELECT * FROM sessions
WHERE parent_session_id is NULL
ORDER BY updated_at DESC;
```

Sub-agent sessions always have `parent_session_id` set (`session_lifecycle.go:44-49`,
`CreateTaskSession`, called from `coordinator_subagents.go:76-77`). `ListAllSessions`
exists without the filter but is used only by `sessions gc`.

*(executed)* — real store + verbatim `useWS.ts:287-297`, fed the exact set of frames the
backend can produce:

```
PASS  parent session is marked busy
PASS  sub-agent session is NEVER marked busy
=> SubAgentBlock's isRunning = busySessions.has(subSessionID) is a constant false
```

**Four consequences.**

1. **The commit's design argument is false, and so is the comment it shipped.**
   `SubAgentBlock.tsx:77-80` reads: *"nothing broadcasts agent_busy when the coordinator
   spawns one, so the client learns busy state only from the 5s list_sessions poll."* The
   first clause is right; the second is not — the client never learns it. This is the same
   class as the nineteenth review's M-1/M-2 and the twentieth's M-2: a comment asserting a
   mechanism the codebase does not have, written one commit after that class was swept.

2. **The `t5`/`t6` step-boundary window is open at every step boundary**, not only during
   a bounded start-up lag. The window itself is short — `OnStepFinish`'s remaining DB work
   (`sessions.Get`, `IncrementCost`, `SetUsage`, `recordMessageUsage`, `IsCancelRequested`)
   plus `PrepareStep`'s `DrainPendingInjects` and `messages.Create`, racing the 16 ms
   `message_updated` batch ticker — so about half the time the client sees the new message
   first and nothing flashes at all (§1.2). But when it does flash, it is not harmless:

3. *(executed)* — verbatim `SubAgentBlock.tsx:124-131`:

   ```
   PASS  mid-stream: block is open
   PASS  operator's manual collapse holds
   PASS  the rising edge silently discarded the manual override
   PASS  …and the block the operator collapsed re-opens by itself
   ```

   A single-frame `done` blink is enough to fire the `prevDone` effect, which sets
   `override = undefined`. So an operator who deliberately collapses a noisy sub-agent
   watches it **pop back open by itself** at a step boundary. That is the one part of this
   finding that is unambiguously visible at human timescales.

4. **Three affordances are unreachable.** `:141` (Bot icon `animate-pulse`), `:143`
   (`{isRunning && …"running..."}`), `:149` (`{messages.length === 0 && isRunning && …
   "Starting agent..."}`). A freshly spawned sub-agent therefore renders an **empty
   expanded box** with no indication anything is happening, and — combined with residual
   (b) — a sub-agent whose process was hard-killed is *pixel-identical* to one that is
   still working. (Partial mitigation, in fairness: `SubAgentMessage:39` shows a pulsing
   "running..." next to each unfinished tool call, so once the first tool call lands there
   is *some* motion.)

**Fix shape (not applied).** There is a signal already on the wire that means exactly
"this sub-agent's run is over", and it is authoritative: the parent message's own
`tool_call` part for the agent tool. `ToolActivityGroup.tsx:194-197` already holds it and
passes `part.ID` and `messageID`; passing `part.Finished` too would make `done` a fact
rather than an inference, and would also fix residual (b) — a hard-killed sub-agent's
tool call is finished by the recovery path. Two alternatives, both worse: teach
`handleListSessions` to use `ListAllSessions` for the busy loop (fixes the poll but leaves
a 5 s lag and grows an unbounded per-poll fan-out), or drop the inert conjunct and refine
on the finish *reason* (`Reason !== "tool_use"`), which is wrong for loop-detected
terminations — `agent_turn.go:1373-1378` keeps `finishReason` at `tool_use` when the loop
detector breaks a tool-calling turn, so such a run would never show done.

Whatever is chosen: `SubAgentBlock.tsx:72-80` must stop claiming the 5 s poll supplies
busy state for sub-sessions.

---

### F-2 (Major / P2) — the lazy `load_messages` reply is dropped, so historical sub-agent blocks stay empty after a reload

**Files:** `web/src/components/SubAgentBlock.tsx:106-120`, `web/src/useWS.ts:139-145,234`.

`SubAgentBlock`'s mount effect exists for one stated reason:

```ts
// :106-108
// Lazy-load sub-agent messages on first mount when nothing is in the
// store yet (the WS handler only auto-loads sub-sessions created during
// the live session — past runs surfaced after a reload start empty).
```

The reply never arrives at the store. `useWS.ts:234` routes `messages_list` by
`isSubAgentSession(sid)`, which reads `$subAgentSessions`, which has exactly two writers:

- `useWS.ts:106` — the `session_created` handler. Fires for a **live** spawn, or for one
  replayed out of the hub's ring buffer on connect (`hub.go:441-448`).
- `useWS.ts:141-145` — the `sessions_list` loop, `for (const s of sessions) if (s.ParentSessionID) …`.
  **This loop is dead code**: by F-1's SQL, `sessions_list` never carries a child. (Its
  siblings `filter(s => !s.ParentSessionID)` at `:147` and `find(s => !s.ParentSessionID)` at `:168`
  are equally vestigial.)

So after a reload, a sub-session whose `session_created` is no longer in the replay ring is
unregistered, and the reply falls through to the main-chat branch, where
`sid !== activeID` returns early. *(executed)* — real store, verbatim `useWS.ts:139-145`
and `:215-243`:

```
PASS  sessions_list registered NO sub-agent session (children are filtered server-side)
PASS  the lazy reply did NOT reach $subAgentMessages  <-- block stays empty forever
PASS  ...and it did not clobber the main transcript either (guard held)
PASS  with a live/replayed session_created the very same reply lands
```

(The third line is the good news: the envelope guard the twentieth review praised does
hold — a dropped sub-agent reply cannot wipe the active transcript.)

**How often is the `session_created` still in the ring?** Rarely. The ring is
`maxBufferSize = 2000` events (`hub.go:20`) shared by *all* broadcasts, and a single
streaming assistant message alone emits a `message_updated` every `batchInterval = 16 ms`
(`events.go:17,86-89`) — on the order of 60 events per second of streaming. The ring
therefore holds roughly the last half-minute of agent activity. A sub-agent's
`session_created` survives a reload only if that sub-agent spawned within about that
window; after a server restart the ring is empty and nothing survives. The practical
statement is: **reload the web UI and every sub-agent block already in the transcript is
an empty box.** The block cannot even say "Starting agent…" about it, per F-1(4).

`ToolActivityGroup.tsx:190-192` notes sub-agent parts are "rare in practice — usually zero
per group", which is a plausible reason this has gone unnoticed.

**Fix shape (not applied).** One line at the call site: register before asking.
`SubAgentBlock` knows `messageID`, and the parent message belongs to the active session,
so `registerSubAgentSession(subSessionID, $activeSessionID.get()!)` immediately before
`ws.send("load_messages", …)` (`:118-119`) makes the reply routable. Passing the owning
`sessionID` down from `ToolActivityGroup` instead of reading the active atom would be
cleaner. Separately, `useWS.ts:141-145` should either be deleted as dead or the server
should start including children — but not silently, since `sessions_list` also drives
`setSessions`, the sidebar and the auto-select logic.

---

### F-3 (Minor) — the deferred modal-handler item is **not** inert: two of the sites call a parent-owned callback and close a *different, later* form

This is the item two rounds declined to file as "a house-pattern inconsistency, not a
defect". I tried to falsify it and could not. The previous framing — "bounded by abort
count, `setState` on an unmounted component is a no-op in React 18+" — is correct for
three of the six sites and **wrong for the other three**, because those handlers do not
only touch their own dead component's state.

**`MCPSettings.tsx:120-128` (`MCPForm.submit`)** and **`ProvidersModal.tsx:159-166`**
(`CustomProviderForm.submit`) both end with:

```ts
const unsub = ws.on("*", (msg) => {
  if (msg.id !== msgID) return;
  unsub();
  setBusy(false);
  if (msg.error) setError(msg.error);
  else onCancel();          // <-- parent-owned; parent is still mounted
});
```

`onCancel` is `() => setShowAdd(false)` (`MCPSettings.tsx:405`) / `() => setEditing(false)`
(`:216`, `ProvidersModal.tsx:616,817`). The **form** unmounts; the **parent** does not. And
the form's Cancel button is not disabled while busy (`MCPSettings.tsx:167-172`; only the submit button carries `disabled={… || busy}`, `:175`), nor is
`Escape` (`:132`).

*(executed)* — driving the **real** `web/src/ws.ts` singleton with a fake socket, verbatim
handler body, parent state modelled as `showAdd`:

```
PASS  form #1 closed while its add_mcp_server was still in flight
PASS  form #2 is open with the operator's typed JSON
PASS  form #2 was closed by form #1's leaked handler  <-- typed JSON discarded
PASS  …and form #1 was long unmounted when it fired
PASS  LogsModal's leaked handler touches nothing outside its own dead component
```

Reachable sequence, all of it ordinary use:

1. Add an MCP server whose command is slow or hangs. `mcp.AddServer`
   (`internal/agent/tools/mcp/init.go:358-385`) calls `initClient` **synchronously** —
   the button literally reads "Connecting…" — so a multi-second wait is the normal case,
   and a bad command means a full connect timeout.
2. Operator gives up, presses Cancel/Escape. Form #1 unmounts; the wildcard handler stays.
3. Operator opens the Add form again and types a different config.
4. The first request finally answers. The leaked handler matches on `msg.id`, and
   `onCancel()` **closes form #2**, discarding what the operator was typing.

A secondary effect on the same path: if the late reply is an *error*, `setError` lands on
the dead form and the failure is swallowed with no notice anywhere.

**`SettingsModal.tsx:150-167` (`handleInitialize`)** is a third non-inert case, of a
different shape: its leaked handler calls `setActiveSession(payload.sessionID)`, a write to
a **global atom**, entirely independent of mount state. `initialize_project` runs a full
agent turn (`handlers_agent.go:515-532`), so the reply can be minutes late — long after
the operator dismissed Settings and moved to another session, at which point the UI yanks
them into the init session. (Note this one fires with the modal open too, so
unsubscribing on unmount is a *scoping* fix, not the whole story.)

**Genuinely inert, and I am closing them out as such** — these three touch nothing but
their own dead component's local state, and React 18 removed the unmounted-setState
warning, so there is no console noise either:

| Site | What the leaked handler does | Why inert |
|---|---|---|
| `LogsModal.tsx:19-28` | `setLogs` / `setError` / `setLoading` | local state only |
| `SettingsModal.tsx:88-99` (`SkillsSection.refresh`) | `setLoading` / `setSnapshot` | local state only |
| `ChatToolbar.tsx:38-45` (`SystemPromptModal`) | `setOriginal` / `setDraft` / `setLoading` | local state only; not a wildcard (`ws.on("system_prompt")`); `onClose` is `useCallback(…, [])` at `ChatToolbar.tsx:142` so the effect does not re-run and re-register |

The listener-count concern is also not real: `emit` (`ws.ts:80-82`) iterates a `Set` keyed
by type, each leaked handler is a single `msg.id !== X` comparison, and the count is
bounded by how many times an operator aborted a modal request. That part of the prior
rounds' reasoning holds.

**So: the class is not "clean this up for consistency". Two sites lose operator input and
one navigates the app.** `ScopedModelsModal.tsx:309-315`'s `return unsub` remains the
correct pattern and fixes all six.

---

### F-4 (Minor) — the deferred `TodoList` race is real; here is the concrete sequence

**File:** `web/src/components/TodoList.tsx:40-57,308-318`.

`TodoRow` is keyed by array index (`key={i}`, `:310`) and holds `draft` in local state
(`:41`). React therefore keeps the *same* component instance — with `editing` and `draft`
intact — mounted at index *i* when the `todos` prop is replaced by a different list of the
same or greater length. `commitEdit` then reads the **new** prop:

```ts
// :53-57
function commitEdit() {
  const trimmed = draft.trim();
  if (trimmed && trimmed !== todo.content) onChange({ ...todo, content: trimmed });
  setEditing(false);
}
```

The agent's `update_todos` reaches this prop live: it writes `session.Todos`, which the
server broadcasts as `session_updated` (`events.go:41-43`), which `upsertSession` feeds
straight into `Chat`'s `TodoList`.

*(executed)* — verbatim `commitEdit` + `changeTodo` (`:218-222`):

```
t0 agent list:  A(done) | B(done) | C(pending) | D(pending)
t1 operator clicks ✏ on index 2 ("C"); draft := "C"
t2 operator types  "C — check the retry path too"
t3 agent update_todos lands: A/B cleared, C in progress, E/F appended
       new list:  C(in_progress) | D | E | F
t4 operator presses Enter

  committed onto: {"content":"C — check the retry path too","status":"pending",…}
  list after    : C | D | C — check the retry path too | F

PASS  the operator's text WAS committed
PASS  …onto task 'E', not onto 'C' — and 'C' keeps its old text
PASS  task 'E' is gone from the list (its text was overwritten)
```

The wrong outcome is concrete and silent: task **E** is destroyed, the edit the operator
actually made to **C** is lost, and nothing indicates either. `changeTodo` then writes the
whole array back via `updateTodos`, so it is persisted.

Blast radius is one todo, and the collision window is the few seconds a row is open for
editing against a periodic agent write — genuinely rare. **Minor**, but it is a real
reproduction, not a "looks fragile", and it should stop being deferred.

**Fix shape (not applied).** Key the rows by identity rather than position — `content` is
not unique enough, so the honest fix is a stable client-side id, or, much cheaper: capture
the todo the edit started on (`const editingRef = useRef(todo)` at `startEdit`) and have
`commitEdit` refuse when `todo !== editingRef.current`. Cancelling the edit on a prop
change would also be defensible and is one line.

---

### F-5 (Minor) — a failed sub-agent renders a green "done" badge and no error

**File:** `web/src/components/SubAgentBlock.tsx:25-49,144`.

`SubAgentMessage` renders exactly two part kinds: `tool_call` names and `text`. It ignores
`finish` entirely — and `return null` at `:32` when there is neither text nor a tool call.
The main renderer does not: `Part.tsx:34-36` is a fork patch whose comment is *"render
explicit error/empty finish parts so a failed turn is [visible]"*, and
`AssistantContent.tsx:62-70` routes an empty finished message to `FinishErrorBlock`.

So a sub-agent that ends on `FinishReasonError` — provider closed the stream empty
(`agent_turn.go:1355-1372`), peak-hours abort (`:1490-1500`), summarisation failure
(`agent_compaction.go:435`) — satisfies `isTerminallyFinished`, and the block shows the
green **"done"** badge at `:144` with the error text nowhere on screen. If the failure
produced no content at all, the body is empty as well: a green "done" over a blank box.

Same family as the twentieth review's M-3 and M-1: `SubAgentBlock` re-implements a slice
of the main renderer and drops a rule. **Fix shape:** read the last assistant message's
finish `Reason` where `done` is already computed (`:93-97`) and render an error badge plus
`FinishErrorBlock` instead of the green one; both components are already importable and
`SummaryMessage` is already reused here, so the precedent for reuse in this file exists.

---

## §3 — Fresh end-to-end read of `SubAgentBlock.tsx`: what is fine

Read line by line, independently of M-1/M-2's shape. Beyond F-1/F-2/F-5, nothing to file:

- **`:68` composite key.** `${messageID}$$${toolCallID}` matches
  `CreateAgentToolSessionID`'s `"%s$$%s"` and the ID `coordinator_subagents.go:76-77`
  actually creates. The `: toolCallID` fallback is unreachable in practice and harmless.
- **`:71` `?? []`** allocates a fresh array each render when the session is absent, so the
  `done` memo re-runs. Deps are `[isRunning, messages]`; the recomputation is O(n) over an
  empty array and the result is stable. Not worth a `useMemo` of its own.
- **`:114-120` lazy-load latch.** Correct as re-justified by M-2: `requested.current` is set
  *before* the send, so an empty sub-session is asked exactly once; the effect's
  `messages.length` dep re-arms it only after `removeSubAgentMessage` empties a
  previously-populated slice, which is one deliberate re-fetch, not a loop.
- **`:126` `useRef(done)`** seeds `prevDone` from the first render, so a block that mounts
  already-done does not fire a spurious override reset.
- **`:36-41` `key={i}` over `toolCalls`.** Unlike F-4 this one is safe: tool-call parts are
  appended and mutated in place (`Message.FinishToolCall`), never reordered or removed, so
  the filtered array's indices are stable.
- **`:156-164` render filter.** `!m.Hidden` + `IsSummaryMessage → SummaryMessage` matches
  `Message.tsx:35-36`, and both compaction paths do create summaries with
  `Role: message.Assistant` (`agent_compaction.go:369-375`, `:562-570`), so the
  role-filter-before-summary-branch ordering difference cannot bite.
- **Store shape.** `upsertSubAgentMessage` (`store.ts:268-279`) replaces in place, so array
  order stays arrival order and `[...messages].reverse().find(assistant)` really is the
  latest assistant message. `created_at` is milliseconds (`migrations/20250424200609_initial.sql:30`),
  so `ORDER BY created_at ASC` ties are not a practical concern for the reload path either.

---

## §4 — Swept, nothing filed

- **`useWS.ts` `messages_list` envelope guard** — re-probed as part of F-2. It holds: a
  dropped sub-agent reply cannot clobber `$messages`, because `activeID` is always set by
  the time a `SubAgentBlock` can mount.
- **`hub.go` replay on register** (`:390-450`) — sticky-first-then-ring ordering, per-event
  and byte budgets, non-blocking sends. Read in full for F-2; correct.
- **`ChatToolbar.tsx`'s `SystemPromptModal` effect deps** — `[sessionID, onClose]` looked
  like a re-registration hazard (the cleanup removes only the keydown listener, not
  `unsub`). It is not: `closeSystemPrompt` is `useCallback(…, [])` at `:142`, so the effect
  runs once per mount.
- **`ProvidersModal`'s `sendAndWait`** (`:390-402`, handler at `:394`) — same leak shape, but it only
  `resolve()`s a promise whose continuation is the dead component's `submit`; no external
  write. Inert.
- **`agent_compaction.go` summary lifecycle** — read for §1.5's third residual; both paths
  stamp a non-Partial `FinishReasonEndTurn` (`:445`, `:628`), so `done` settles correctly
  once the summary completes.

---

## §5 — Build / check state at HEAD

- `pnpm typecheck` (`tsc --noEmit`) — clean.
- No Go package modified by this review; no Go tests run (nothing in scope to scope them
  to, and the commit under review touches no Go).
- Working tree unchanged by this review. The harness is in `%TEMP%/crush-r21/`, not in
  the repository.

---

## §6 — Recommendation to the orchestrator

Priority order, and they are not equal:

1. **F-2 first.** It is the cheapest fix in the list (register the sub-session before the
   lazy load) and the one an operator meets every time they reload the page. It also
   makes F-1's dead "Starting agent…" placeholder worth reviving.
2. **F-1 second**, and it needs a decision, not just a patch: either give `SubAgentBlock`
   the parent `tool_call` part's `Finished` flag (my recommendation — it is authoritative,
   already in hand at `ToolActivityGroup.tsx:194-197`, and closes the hard-kill residual
   too), or supply a real busy signal for sub-sessions. Either way, delete or correct the
   `:77-80` comment in the same commit — it currently documents a poll that cannot see
   sub-sessions. F-1 and F-2 share a root cause and can reasonably go in one commit.
3. **F-3** — mechanical: `return unsub` from an effect in all six modal sites, matching
   `ScopedModelsModal.tsx:309-315`. Two of them (`MCPForm`, `CustomProviderForm`) are the
   ones that actually lose operator input; do those even if the rest are deferred.
4. **F-5** and **F-4** are the small ones and can wait, but neither should be deferred a
   *third* time on the grounds of "below the bar" — both now have executed reproductions.

**On the exit condition.** This round is not it, and I want to be plain about why, because
the previous round predicted it might be. The twentieth review's recommendation was
followed exactly and the two deferred items were resolved — both turned out to be real
defects, which is the outcome that recommendation existed to determine. F-1 and F-2 are new
ground opened by verifying `407b5f3a`'s *rationale* rather than its *behaviour*: the
predicate the commit ships is correct on every input, but the argument for its shape rested
on a signal that does not exist, and checking that claim to the SQL is what surfaced both.

The honest read of twenty-one rounds is that `SubAgentBlock.tsx` and its wiring into
`useWS`/`store` is the last part of `web/` that has never had a from-scratch audit — every
round has approached it through whichever defect the previous round found. A twenty-second
round that fixes F-1 through F-5 and then re-reads **`useWS.ts`'s routing predicates as a
set** (`isSubAgentSession`, the `activeID` gates, the `sessions_list` consumers) has a
better chance of coming back empty than another pass over `SubAgentBlock` alone —
because both of this round's Major findings are routing defects that happen to *surface*
in that component.
