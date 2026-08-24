# Twenty-second review — `9c1d661b..04b62d59`

**Scope, as instructed:**

1. Independent verification of all three commits, including re-execution of the two
   `24e556f5` sequences and the `434ef501` reproduction that the orchestrator accepted
   on the worker's transcript.
2. The twenty-first review's own closing recommendation: audit `useWS.ts`'s routing
   predicates **as a set**, cross-referenced against `internal/server/events.go`'s
   actual broadcast scope, hunting specifically for a FOURTH consumer of the
   "sub-agent sessions are invisible to X" root cause.

**Out of scope by instruction, and honoured:** `handlers_agent.go`'s
`handleRerunMessage` / `recreateRerunPromptIfLost` (not opened). The backend sweep
closed by the nineteenth review was not re-opened as a sweep — backend files were read
only where a specific `web/` question required knowing what the server puts on the wire
or what a flag means (`agent_turn.go`, `app_recovery.go`, `events.go`, `hub.go`,
`handlers_sessions.go`, `handlers_messages.go`, `message/content.go`,
`session_read.go`, `session_lifecycle.go`, `db/sql/sessions.sql`, `history`, and
fantasy v0.25.2's `agent.go`).

**Date:** 2026-08-24
**Reviewed at:** `04b62d59` (working tree: the known ` D web/dist/.gitkeep` plus
untracked `docs/`; nothing else. This review changed nothing in the repo — both
harnesses live in the OS temp dir).

---

## Verdict

**NO-GO on `04b62d59` as shipped. GO on `434ef501` and `24e556f5`.**

`434ef501` and `24e556f5` are both correct and both independently re-verified by
execution against pre-fix and post-fix source; nothing to file against either.

`04b62d59` must not ship in its current form. Its central premise — that the parent
`tool_call` part's `Finished` flag means *"the agent tool returned, i.e. the sub-agent
run is over"* — is false. `Finished` means **"the model finished emitting the tool
call's arguments"**, and it is set *before the tool is dispatched*, which is before the
sub-agent session is even created. I proved this by running fantasy v0.25.2's own step
loop with a stubbed language model and a sleeping `agent` tool: `part.Finished` becomes
`true` at 0 ms, the tool's `Run()` starts at 1 ms, and it returns at 1201 ms.

The consequence is a strict regression on the twenty-first review's F-1. `done` is now
`true` for the entire sub-agent run instead of flapping at step boundaries: the block
**self-collapses about a second into every delegation and wears a green "done" badge
for however many minutes the sub-agent actually runs**. `isRunning` is `!done`, so all
three affordances F-1 set out to revive (the Bot pulse, the "running…" badge,
"Starting agent…") are dead again — now permanently rather than merely often. Before
`04b62d59` the block at least stayed open and streamed the sub-agent's transcript.

Pulling that thread found the fourth "sub-agent sessions are invisible to X" the task
asked for, and it is not in `useWS.ts` at all: **`app_recovery.go`'s startup orphan
sweep iterates `Sessions.List`, whose SQL filters children out**, so a sub-agent's own
transcript is never repaired — its doc comment says "For every session". That one
interlocks with the first finding to make a hard-killed sub-agent render as a plain
green "done".

The `useWS.ts` routing audit itself came back essentially clean: no misroute, no
double-route, no gap in the four `isSubAgentSession` consumers. It did surface one
broadcast that no client handles at all, and confirmed one piece of dead code the
twenty-first review flagged and this round left in place.

**Findings: 1 Major (P2, a regression in the commit under review), 4 Minor.** No P1.

---

## §0 — Harnesses

Two, both outside the repository, both marked *(executed)* below.

**Go** — `%TEMP%/crush-r22/gostub/`, a standalone module depending only on
`charm.land/fantasy v0.25.2` (the version `go.mod` pins). It stubs a `LanguageModel`
that streams `tool_input_start → tool_input_delta → tool_input_end → tool_call →
finish(tool-calls)` for one `agent` tool call, registers the same callbacks crush
registers in `agent_turn.go`, and gives the `agent` tool a 1200 ms `Run()`. It records
the real-time order of every callback and of the tool's own entry/exit.

**Node 24** — `%TEMP%/crush-r22/js/`. Imports the **real** `web/src/store.ts`,
`web/src/ws.ts` and `web/src/components/Message/textParts.ts` by absolute file URL,
with browser globals stubbed before load and a `module.registerHooks` resolver for the
extensionless relative specifiers the bundler normally handles. Where a component's
logic is under test, the harness **extracts the function's source text out of the .tsx
file by brace-matching** (`extract.mjs`) and compiles it — at both `407b5f3a` (via
`git show`) and the working tree — so the assertions run the repository's own code, not
a transcription of it. That applies to `TodoList`'s `startEdit`/`commitEdit`/
`cancelEdit`/`changeTodo`, `MCPSettings`' `MCPForm.submit`, `SettingsModal`'s
`handleInitialize`, and **every `ws.on` handler body in `useWS.ts`**. `SubAgentBlock`'s
predicates are the one exception: they are expression-level, not function-level, so
they are transcribed verbatim with line citations.

Tally:

```
test_subagent.mjs   13 passed, 0 failed
test_todo.mjs       10 passed, 0 failed
test_leaks.mjs      13 passed, 0 failed
test_routing.mjs    18 passed, 1 failed   <- the one failure is F-4
```

`pnpm typecheck` (`tsc --noEmit`) clean at `04b62d59`. No Go package was modified by
this review, so no Go tests were run beyond the standalone harness above.

---

## §1 — Verification of the three commits

### 1.1 `434ef501` (TodoList) — verified, nothing to file

*(executed)* — real `startEdit`/`commitEdit`/`cancelEdit` bodies extracted from
`web/src/components/TodoList.tsx` at both revisions, driven through the twenty-first
review's exact sequence, with the real `changeTodo` applying whatever the row commits:

```
[pre-fix 407b5f3a]  committed: {"content":"C — check the retry path too","status":"pending",…}
[pre-fix 407b5f3a]  list after: C | D | C — check the retry path too | F
PASS  pre-fix: the edit WAS committed
PASS  pre-fix: it landed on 'E', not on 'C'
PASS  pre-fix: task 'E' is destroyed
PASS  pre-fix: 'C' keeps its old, unedited text

[post-fix WORKTREE]  committed: null
[post-fix WORKTREE]  list after: C | D | E | F
PASS  post-fix: NOTHING was committed
PASS  post-fix: no write was issued at all
PASS  post-fix: 'E' survives
```

The worker's claim that the fix does not over-cancel also holds, and I probed the case
its commit message argues about explicitly — object-identity churn from
`upsertSession` replacing the whole session on an unrelated `session_updated`:

```
PASS  post-fix: an untouched row still commits normally
PASS  post-fix: object-identity churn alone does NOT cancel the edit
```

One behaviour worth recording rather than filing, because it is the guard working as
designed rather than a defect: if the agent flips the **same** todo's status without
touching its text, the content comparison passes and the commit goes through carrying
`{...todo}` — i.e. the operator's new text **plus the agent's new status**. That is the
right merge, and I confirmed it:

```
PASS  post-fix: a same-content status flip still commits, and preserves the agent's new status
```

### 1.2 `24e556f5` (one-shot `ws.on` handlers) — verified, nothing to file

*(executed)* — the real `ws.ts` singleton with a fake socket, and the real `submit` /
`handleInitialize` bodies extracted at both revisions. Both sequences the orchestrator
took on trust reproduce pre-fix and close post-fix:

```
[pre-fix]  cleanup present in source: false; form #2 still open: false
PASS  pre-fix: form #1's handler survived its unmount
PASS  pre-fix: the late reply CLOSED form #2 (typed JSON discarded)

[post-fix] cleanup present in source: true; form #2 still open: true
PASS  post-fix: form #2 is still open, typed JSON intact

[pre-fix]  active session after reply: sess-init-created
PASS  pre-fix: the late reply overwrote $activeSessionID
PASS  pre-fix: ...and called onClose on a modal that was already gone

[post-fix] active session after reply: sess-the-operator-chose
PASS  post-fix: $activeSessionID is still the operator's choice
PASS  post-fix: onClose was not called after unmount
```

Regression probes I added that the worker did not enumerate — the fix must not break
the paths it guards:

```
PASS  post-fix: a reply to a LIVE form still closes it
PASS  post-fix: an error reply still surfaces on the live form
PASS  post-fix: five abandoned forms leave zero live handlers behind
```

The `unsubRef.current?.()` call the fix adds *before* each new registration is also
correct and not redundant: it covers a second submit from the same still-mounted form
(retry after an error), which would otherwise orphan the first registration beyond the
ref's reach. The seventh site the worker found on its own
(`ProvidersModal`'s `BuiltinProviderEditor.sendAndWait`) is genuinely a different
shape and the `Set` is the right container — `sendAndWait` is called twice
concurrently from `submit`, so a single ref would drop one.

### 1.3 `04b62d59` (SubAgentBlock) — see F-1

The F-2 half (register the sub-session before the lazy `load_messages`) and the
`errorFinish` structure are both fine in isolation and are confirmed working in §2's
routing table (R-2). The `done` half is wrong; see F-1.

---

## §2 — The `useWS.ts` routing audit, as a set

Every `ws.on` handler body in `useWS.ts` was extracted from the file and executed
against the real store. The table below is the result, not a reading.

| event | identity check | store consulted | what happens on a miss | verdict |
|---|---|---|---|---|
| `_connected` | none | — | resets `$busySessions`, re-requests `list_sessions`/`config`/`skills` | fine |
| `_disconnected` | none | — | — | fine |
| `session_created` | `s.ParentSessionID` (payload — authoritative, straight off the DB row) | — | n/a | fine |
| `session_updated` | **none** | — | a child session is upserted into `$sessions` unfiltered | inert; see §3 |
| `session_deleted` | none | — | sub-agent maps are never pruned | inert; see §3 |
| `sessions_list` | `s.ParentSessionID` | — | the `:141-145` registration loop cannot fire | **F-5** |
| `message_created` | `isSubAgentSession` | `$subAgentSessions` | dropped (falls to the `activeID` gate, which a sub-session ID never satisfies) | fine |
| `message_updated` | `isSubAgentSession` | `$subAgentSessions` | dropped, same way | fine |
| `message_deleted` | `isSubAgentSession` | `$subAgentSessions` | dropped, same way | fine |
| `messages_list` | `isSubAgentSession`, then `activeID` | `$subAgentSessions` | falls to the main branch; the `SessionID` envelope guard blocks the clobber | fine |
| `agent_busy` | `p.SessionID` | `$busySessions` | a sub-session ID can never be named | fine (no longer consumed) |
| `summarize_queued` | `p.SessionID` | `$summarizeQueued` | same | fine |
| `config` / `mcp_state` / `skills` / `update_available` / `error` | none | — | — | fine |
| `file_updated` | — | — | **no handler exists anywhere in `web/`** | **F-4** |

*(executed)* — the load-bearing rows:

```
R-1  PASS  unregistered sub-session: message_created/updated go nowhere
     PASS  registered sub-session: message_created routes to $subAgentMessages
     PASS  registered sub-session: message_deleted removes in place
     PASS  foreign top-level session: message_created dropped
     PASS  active session: message_created lands in $messages

R-2  PASS  unregistered sub reply: NOT stored in $subAgentMessages
     PASS  unregistered sub reply: envelope guard protects $messages
     PASS  registered sub reply: lands in $subAgentMessages   <- 04b62d59's F-2 fix works
     PASS  registered sub reply: $messages untouched

R-3  PASS  sessions_list registered ZERO sub-agent sessions (payload is child-free by SQL)
     PASS  the :141-145 loop DOES fire — but only on a payload the server never sends

R-4  PASS  parent session is marked busy
     PASS  no sub-agent session ID can reach $busySessions

R-7  broadcast-but-unhandled: ["file_updated"]
```

**Conclusion of the audit itself: the classification is sound.** There is no misroute,
no double-route and no race in it. Every event that carries a session identity checks
it against the right store, and the one place where a miss could have damaged state
(`messages_list` overwriting the active transcript) is guarded by the server-side
envelope and by `activeID`. `04b62d59`'s registration-before-load fix closes the last
reachable miss.

**The fourth "invisible to X" is not in `useWS.ts`.** Having cleared the client, I
followed `Sessions.List`'s child-filtering SQL to every other consumer that means
"every session". Most are correct by intent (`sessions list/cost/grep/pick/watch/
cancel` are top-level views by design, and the fork has `ListSubSessions` +
`sessions subagents`/`sessions tree` for children). One is not: the startup orphan
recovery sweep. That is **F-3**.

Two near-misses I checked and cleared, both of which would have been fifth instances:

- **Permissions.** `autoApproveWebSession` arms only the top-level session, but
  `permission.go:389-408`'s `InheritSessionAutoApprove` is an explicit parent→child
  inheritance helper that `coordinator_subagents.go:107` calls right after
  `CreateTaskSession`. Correct.
- **`DurationBadge` inside a sub-agent block.** `SubAgentBlock` renders
  `SummaryMessage` for a sub-agent's compaction summary, and `SummaryMessage:40`
  renders `DurationBadge`, which does `busy.has(message.SessionID)` — a lookup that can
  never succeed for a sub-session. Inert: `SummaryMessage` only mounts the badge under
  `isFinished`, and `DurationBadge:20` is `!isFinished && busy.has(...)`, so the dead
  lookup is already short-circuited.

---

## §3 — Findings

### F-1 (Major / P2 — regression) — `SubAgentBlock`'s `finished` prop means "the model finished typing the tool call", not "the sub-agent finished"

**Files:** `web/src/components/SubAgentBlock.tsx:56,61,113-115,193,195,196-200,205-207`,
`web/src/components/Message/Part.tsx:26`,
`web/src/components/Message/ToolActivityGroup.tsx:197`.
**Root cause:** `internal/agent/agent_turn.go:1224-1231` and `:1242-1270`.

`04b62d59` makes `done = finished || parentDone` where `finished` is the parent
message's agent `tool_call` part's `Finished` flag, on the stated grounds that it is
"true exactly when the agent tool returned, which is the whole sub-agent run"
(`SubAgentBlock.tsx:89-93`). It is not. `ToolCall.Finished` is set in two places, both
of which run **while the provider stream is still being consumed, before any tool is
dispatched**:

```go
// internal/agent/agent_turn.go:1224-1231
OnToolInputEnd: func(id string) error {
        bumpActivity()
        sessionLock.Lock()
        currentAssistant.FinishToolCall(id)          // <- Finished = true
        …
},
// internal/agent/agent_turn.go:1256-1264
toolCall := message.ToolCall{
        ID: tc.ToolCallID, Name: tc.ToolName, Input: input,
        ProviderExecuted: false,
        Finished:         true,                      // <- Finished = true, again
}
sessionLock.Lock()
currentAssistant.AddToolCall(toolCall)
```

The second one is inside `OnToolCall`, and the fork's own comment four lines above it
says so plainly: *"fantasy fires every OnToolCall for a step before executing any
tool, so the counter brackets the whole executeTools window"* (`:1245-1249`). fantasy
v0.25.2 confirms it structurally — `OnToolCall` is invoked inside the
stream-consumption loop (`agent.go:1446-1462`) while dispatch is deliberately buffered
until the stream is exhausted (`:1471-1476`, *"Buffer dispatch until stream is fully
consumed so that all OnToolCall callbacks complete before any tool result is
written"*, flushed at `:1576-1580`).

*(executed, Go)* — stub model + a 1200 ms `agent` tool, real-time order:

```
     0ms  model: stream tool_input_start
     0ms  OnToolInputStart(call-1) -> crush AddToolCall{Finished:false}
     0ms  model: stream tool_input_delta
     0ms  model: stream tool_input_end
     0ms  OnToolInputEnd(call-1)   -> crush FinishToolCall  => part.Finished = TRUE
     0ms  model: stream tool_call
     0ms  OnToolCall(call-1)       -> crush AddToolCall{Finished:true} => part.Finished = TRUE
     0ms  model: stream finish(tool-calls)
     1ms  >>> agent tool Run() STARTED  (sub-session is created here)
  1201ms  <<< agent tool Run() RETURNED (sub-agent finished)
  1201ms  OnToolResult(call-1)     -> crush creates the role=tool message
  1201ms  OnStepFinish(tool-calls) -> crush AddFinish (non-Partial) on the PARENT message

part.Finished became TRUE at 0ms
```

Both writes are persisted before dispatch (`messages.Update` at `:1230` and `:1269`),
so the client sees `Finished: true` on the wire well before the sub-agent exists.

**What the operator sees.** *(executed, Node — verbatim `SubAgentBlock.tsx:113-115` and
the `:176-183` open-state machine, over the wire timeline the Go harness just
established):*

```
 phase                                            finished  done  isRunning  open  badge
 t0  parent assistant created, empty              false     false true       true  running...
 t1  OnToolInputStart  (args still streaming)     false     false true       true  running...
 t2  OnToolInputEnd/OnToolCall  (args complete)   true      true  false      false done(green)
 t3  agent tool Run() starts, sub-session created true      true  false      false done(green)
 t4  sub-agent streaming, 60s in                  true      true  false      false done(green)
 t5  2s checkpoint ticker stamps a Partial finish true      true  false      false done(green)
 t6  sub-agent streaming, 180s in                 true      true  false      false done(green)
 t7  agent tool RETURNS -> OnStepFinish AddFinish true      true  false      false done(green)

PASS  post-fix: `done` is TRUE while the sub-agent is still running
PASS  post-fix: it goes wrong at t2, before the sub-agent session even exists
PASS  while the args stream, the block is open and reads 'running...'
PASS  the instant the args finish, the block COLLAPSES and reads 'done'
PASS  ...and stays collapsed+done for the entire sub-agent run
```

The collapse is not incidental: `open = override ?? !done` (`:177`) and the `prevDone`
effect (`:179-182`) fires on the rising edge, so the block folds itself shut about a
second into every delegation and reports success while the delegation runs.

**This is worse than what it replaced.** Under `407b5f3a`, `$busySessions` was dead
(twenty-first review F-1) so `done` reduced to the sub-agent's own last-assistant
terminal finish — false at the start of a run, true only at step boundaries. The block
stayed open and streamed:

```
PASS  pre-fix: block was OPEN while the sub-agent streamed (done=false)
PASS  post-fix: the same two moments now render collapsed+done
```

And the three affordances F-1 existed to revive are dead again, now unconditionally:
`isRunning = !done` is permanently `false`, so `:193`'s Bot pulse, `:195`'s "running…"
and `:205-207`'s "Starting agent…" never render for a live sub-agent.

**Why the verification missed it.** The orchestrator re-derived the crux claim by
grepping for `FinishToolCall` call sites and confirming there is exactly one,
`agent_turn.go:1227`. That count is correct. What was not checked is *which callback
line 1227 sits in* — it is `OnToolInputEnd`, not, as the commit message says, "the
normal tool-return path". A second gap: `FinishToolCall` is not the only writer of the
flag; `AddToolCall` at `:1264` sets `Finished: true` directly, and a grep for
`FinishToolCall` cannot see it. So both halves of the commit's stated rationale are
false — it is the tool-input-end path, and a hard-killed sub-agent's tool call **is**
marked finished, before the kill.

**Fix shape (not applied).** The signal that actually means "the agent tool returned"
is the matching `tool_result` part, and it is already at the render site.
`Chat.tsx:194-201` pushes every `tool_result` of a role=`tool` message into the same
burst, and `ToolActivityGroup.tsx:138-148` already routes an `agent`-named
`tool_result` into `rawAgentParts` — where `renderAgents` (`:193-200`) currently drops
it on the floor. *(executed)*:

```
PASS  tool_result-based signal: NOT done during the run
PASS  tool_result-based signal: done once the agent tool returns
```

Two viable shapes:

- **At the render sites:** replace the `finished` prop with
  `items.some(i => i.part.type === "tool_result" && i.part.ToolCallID === part.ID)`.
  Cheap in `ToolActivityGroup`; `Part.tsx` would need the same lookup over `$messages`
  because the result lives on a *different* message.
- **Inside `SubAgentBlock`, my recommendation:** drop the prop entirely and derive it
  from `$messages`, which the component already subscribes to for `parent` — scan for
  a `tool_result` part whose `ToolCallID === toolCallID`. That works identically at
  both render sites and removes a prop whose name invites exactly this misreading.

Keep `parentDone` either way: it is the only signal for a hard kill (no tool result
will ever arrive) — see F-3 for why it must not be the *only* one that matters, and
why the `errorFinish` fallback needs its condition changed from `!finished` to
"no tool result" at the same time.

Whatever is chosen, `SubAgentBlock.tsx:85-112`'s comment block must be rewritten: it
currently asserts that `finished` "arrives via the parent's own `message_updated` and
never flips at the sub-agent's internal step boundaries" — true, and irrelevant,
because it was already true before the sub-agent started.

---

### F-2 (Minor) — the same misreading of `ToolCall.Finished` in three sibling sites, pre-existing

**Files:** `web/src/components/Message/ActionRow.tsx:168,190`,
`web/src/components/Message/ToolCallBlock.tsx:63`,
`web/src/components/SubAgentBlock.tsx:40`.

Filed separately from F-1 because these are **not** regressions — they predate the
commit range — but they share its root cause exactly, and a fix for F-1 that does not
touch them leaves the codebase split between two readings of the same flag.

```tsx
// ActionRow.tsx:168
const running = !!call && !call.Finished && !result;
// ToolCallBlock.tsx:63
{!finished && <span data-test-id="tool-call-running" …>running…</span>}
// SubAgentBlock.tsx:40  (SubAgentMessage, one row per sub-agent tool call)
{!tc.Finished && <span className="animate-pulse">running...</span>}
```

Given F-1's timeline, `!call.Finished` is true only while the model is *typing the
arguments* and false for the whole time the tool actually runs. So every per-row
"running…" badge in the transcript flashes for a fraction of a second at the wrong
moment and is absent for the 60 seconds a `bash` or `agent` call is genuinely
executing. (Group-level motion survives: `ToolActivityGroup:247`'s "live" badge keys
off session busy-ness, not this flag.)

`ActionRow:168` is half-right already — its `&& !result` conjunct **is** the correct
predicate. Dropping `!call.Finished` from that line fixes it, and is the same one-line
change the other two sites need in their own idiom. Low blast radius, but it is the
cheapest possible confirmation that the F-1 fix chose the right signal.

---

### F-3 (Minor) — the fourth "invisible to X": startup recovery never repairs a sub-agent's own transcript

**Files:** `internal/app/app_recovery.go:25-27,61`
(root cause `internal/session/session_read.go:108-118` →
`internal/db/sql/sessions.sql:84-88` / `internal/db/sessions.sql.go:362-366`).

`recoverInterruptedTurns` is the startup safety net for the "silent dying" pattern. Its
doc comment states the contract:

```go
// For every session, it finds the LAST assistant message and, if it has no
// finish part, adds a FinishReasonError marking it as a process-restart
// interruption. Cheap (O(sessions × 1 query each)), non-fatal on error,
// silent when there is nothing to recover.
```

It is not every session. `:61` calls `app.Sessions.List(ctx)` → `service.List` →
`db.ListSessions`, whose SQL is `SELECT … FROM sessions WHERE parent_session_id is
NULL`. Every sub-agent session has `parent_session_id` set
(`session_lifecycle.go:44-56`, `CreateTaskSession`). `ListAllSessions` and
`ListSubSessions` both exist; neither is used here.

So when a process is killed mid-delegation:

- the **parent** session is top-level, gets swept, and its last assistant message
  receives `finish(error, "Process restarted")` — this is the signal `04b62d59`'s
  `parentDone` correctly relies on;
- the **sub-agent** session is a child, is never enumerated, and its trailing
  unfinished assistant message stays unfinished in the database forever. No later
  startup will ever fix it.

Blast radius today is genuinely small, and I want to be plain about that: nothing in
the web UI or the CLI derives a *status* from a sub-session's message finish state
(`sessions list`/`why` status only over top-level sessions; `sessions watch` tails
child messages without statusing them; `SubAgentMessage` ignores `finish` parts
entirely). The reason it is worth filing anyway is that it is the third component of a
three-way interlock with F-1:

*(executed)* — verbatim `errorFinish` memo (`SubAgentBlock.tsx:125-140`) against a
hard-killed delegation after the next startup:

```
PASS  hard kill: the parent's 'Process restarted' error is NOT attributed (branch 2 needs !finished)
PASS  hard kill: the block therefore shows the GREEN 'done' badge
PASS  ...and it WOULD have shown the error had `finished` meant what the commit believed
```

Branch 1 of `errorFinish` looks at the sub-agent's own last assistant message — which
F-3 guarantees carries no terminal error finish (either nothing at all, or a `Partial`
checkpoint that `!p.Partial` filters out, or a stale `tool_use` from a completed
earlier step). Branch 2 looks at the parent's — but is gated on
`!finished`, which F-1 guarantees is never true. So the twenty-first review's F-5
("a failed sub-agent renders a green done badge and no error") is **re-opened for the
hard-kill case specifically**, by the commit that was meant to close it.

**Fix shape (not applied).** Either switch `:61` to a children-inclusive enumeration
(`ListAll` exists and is already used by `sessions gc`), or correct the doc comment to
say "every top-level session" and accept the gap deliberately. If the enumeration
changes, note that the sweep's expensive part is the per-session `Messages.List`, so
the O() in the same comment changes with it. Independently of that choice,
`errorFinish`'s second branch should be gated on "no tool result for this call" rather
than `!finished`, so the parent's recovery finish is attributed whenever the tool
demonstrably never returned.

---

### F-4 (Minor) — `file_updated` is broadcast to every client, with full file contents, and no client has ever handled it

**Files:** `internal/server/events.go:96-111`, `internal/server/protocol.go:23`
(consumer: none — `grep -rn "file_updated\|FileUpdated" web/` returns nothing).

Surfaced by the routing audit's completeness check (R-7), which enumerates every
`h.Broadcast` event type in `internal/server` and every `ws.on` type in the client:

```
broadcast-but-unhandled: ["file_updated"]
FAIL  no backend broadcast is silently discarded by every client
```

`subscribeAndBroadcast` forwards every `history.File` pubsub event to the hub. The
payload is the struct verbatim (`internal/history`), including
`Content string` — the **entire file body**. The edit tool creates two versions per
edit (old content then new, `tools/edit.go:265-269`), so one edit to a 100 KB file
pushes roughly 200 KB to every connected browser tab, where `emit(msg.type)` finds no
handlers and `emit("*")` hits only the modal wildcards, which check `msg.id` and
return.

Three costs, none catastrophic, all avoidable by deleting one goroutine:

1. Bandwidth: proportional to everything the agent writes, per client.
2. **Replay-ring pressure.** `hub.go:20` caps the ring at 2000 events and `:74` at
   16 MiB total; anything under `:83`'s 1 MiB per-event ceiling is *stored*. So file
   writes evict genuine history from the buffer new clients replay — which is exactly
   the resource whose shortness caused the twenty-first review's F-2. That fix is now
   client-side and no longer depends on the ring, but the pressure is real for every
   other replayed event.
3. Full source contents cross the WS to any authenticated client for a feature that
   does not exist.

**Fix shape (not applied):** drop the `a.History.Subscribe` goroutine (`events.go:96-111`)
and the `EventFileUpdated` constant, or — if a diff view is planned — narrow the wire
payload to `{ID, SessionID, Path, Version}` and let the client fetch content on
demand. The former is one deletion; the latter needs a wire type.

---

### F-5 (Minor / nit) — `useWS.ts:141-145`'s sub-agent registration loop is still unreachable dead code

**Files:** `web/src/useWS.ts:141-145` (and, for the same reason,
`internal/server/handlers_sessions.go:131`'s `s.ParentSessionID != ""` skip).

```ts
for (const s of sessions) {
  if (s.ParentSessionID) {
    registerSubAgentSession(s.ID, s.ParentSessionID);
  }
}
```

The twenty-first review identified this as dead (its F-2) and recommended "either
delete it as dead or have the server start including children". `04b62d59` took a third
route — registering from `SubAgentBlock` — which is the right call, but left the loop
in place. *(executed)*:

```
PASS  sessions_list registered ZERO sub-agent sessions (payload is child-free by SQL)
PASS  the :141-145 loop DOES fire — but only on a payload the server never sends
```

Its siblings at `:147` (`filter(s => !s.ParentSessionID)`) and `:168`
(`find(s => !s.ParentSessionID)`) are vestigial in the same way, though harmless as
defensive filters. Filing this at round twenty-two only because it is exactly the class
of "code that documents a mechanism the system does not have" the nineteenth through
twenty-first rounds have been clearing, and because leaving it invites a future round
to re-derive the same SQL fact from scratch. One-line delete; if it is kept as
defence-in-depth, a comment saying so would do.

---

## §4 — Swept, nothing filed

- **`messages_list` envelope guard.** Re-probed end to end. `handleLoadMessages`
  (`handlers_messages.go:108-111`) always wraps with `SessionID`, including for an
  empty list, and the client's `sid !== activeID` early return holds. A dropped
  sub-agent reply cannot wipe the active transcript.
- **Reconnect without a `load_messages` refresh.** On `_connected` the client asks only
  for `list_sessions`, and `sessions_list` returns at `:163` without re-fetching
  messages when `activeID === hashID` — so the transcript is never explicitly
  re-read after a reconnect. Self-healing, so not a finding: the hub replays its ring,
  every `message_updated` carries a full message snapshot, and `upsertMessage`
  appends an unknown ID rather than requiring a prior `message_created`. Only a
  `message_deleted` evicted from the ring could leave a stale row.
- **`session_updated` upserting children into `$sessions`.** *(executed)* — a child
  really does land there, prepended, until the next 5 s poll evicts it. Inert:
  `Sidebar.tsx:16` filters `ParentSessionID` and every other `$sessions` consumer
  (`Chat`, `ChatInput`, `ChatToolbar`, `sitter.ts`, `store.ts`) keys by ID.
- **`session_deleted` not pruning `$subAgentSessions`/`$subAgentMessages`.**
  *(executed)* — confirmed, and confirmed harmless: session IDs are unique, so a stale
  registration can never capture a future session's events. Unbounded-ish memory in a
  very long-lived tab; not worth a fix.
- **`session_created` for a top-level session calling `setActiveSession`
  unconditionally.** A second browser tab creating a session yanks the first one away
  and clears its `$messages`. Real, but long-standing, single-operator-tool behaviour,
  and squarely inside the multi-client problem space `CLAUDE.md` tells us not to import
  upstream's answers for. Recording it, not filing it.
- **Permission inheritance for sub-agents** — `permission.go:389-408` +
  `coordinator_subagents.go:107`. Correct; see §2.
- **`DurationBadge` inside `SubAgentBlock`'s `SummaryMessage`** — dead
  `$busySessions` lookup, already short-circuited by `isFinished`. See §2.
- **`AddFinish` producing multiple finish parts.** `message/content.go:501-510`
  deletes the existing `Finish` before appending, so `errorFinish`'s `find` can never
  pick up a stale `tool_use` finish ahead of a terminal `error` one.
- **`SubAgentBlock`'s lazy-load effect re-running ~60×/s.** Its `parent` dep changes
  identity on every `message_updated` for the parent message, so the effect body runs
  at streaming cadence — but the `requested.current === subSessionID` latch returns on
  the first line. Measured cost is one string comparison per frame. Not worth a
  `useMemo`.
- **`ToolActivityGroup`'s `messageID ?? ""` fallback** (`:197`) and
  `SubAgentBlock`'s `: toolCallID` fallback (`:71`). Both unreachable: every producer
  of `items` (`Chat.tsx:257`, `AssistantContent.tsx:104`) sets `messageID`. Worth
  noting only because `04b62d59` now makes that path *register* a session ID rather
  than merely look one up — a phantom registration would be inert, but the fallback is
  one more reason to prefer deriving state from `$messages` over threading props.

---

## §5 — Recommendation to the orchestrator

1. **F-1 first, and it needs a decision rather than a patch.** `04b62d59` should not
   remain on `main` in its present form: the block is strictly less useful than it was
   at `407b5f3a` for the entire duration of every delegation. My recommendation is to
   derive "the agent tool returned" inside `SubAgentBlock` from `$messages` (a
   `tool_result` part whose `ToolCallID === toolCallID`) and delete the `finished`
   prop, rather than to revert — the F-2 registration fix and the `errorFinish`
   structure in the same commit are both correct and worth keeping. Whichever way it
   goes, rewrite `SubAgentBlock.tsx:85-112`; it currently argues for the wrong signal
   at length.
2. **F-3 in the same commit as F-1**, because the two interlock: `errorFinish`'s
   parent-attribution branch must move off `!finished` at the same moment `finished`
   changes meaning, or the hard-kill case stays broken either way. The recovery-sweep
   half (children-inclusive enumeration, or an honest doc comment) can go separately.
3. **F-2** is three one-line changes and is the cheapest way to prove F-1's chosen
   signal is the house's signal everywhere.
4. **F-4** and **F-5** are independent deletions and can go in any order, or together.

**On the exit condition.** This round is not it, and unlike round twenty-one the reason
is not that a deferred item finally came due — it is that a fix landed on a false
premise about backend semantics, and the premise was checked one hop short. The
orchestrator verified that `FinishToolCall` has exactly one call site; the fact that
mattered was *which callback that site is in*, and that fact was two lines of context
away in the same file, with the fork's own comment at `agent_turn.go:1245-1249`
spelling it out.

The `useWS.ts` audit the twenty-first review asked for **did** come back clean, and I
think that closes it: the routing predicates are now verified as a set, by execution,
against the backend's actual broadcast scope, and the only things left in that file are
one dead loop (F-5) and one unhandled broadcast (F-4). I would not spend a
twenty-third round there.

Where a twenty-third round should look, if F-1 through F-5 land cleanly: the
`ToolCall.Finished` misreading (F-1/F-2) is the second time in three rounds that a
`web/` component has been built on a confident but wrong belief about what a backend
field means (`$busySessions` was the first). Both were found by chasing a *rationale*
rather than a *behaviour*. A short, targeted pass that takes each wire field the web UI
branches on — `Finished`, `Partial`, `Hidden`, `IsSummaryMessage`, `OwnedExternal`,
`Reason` — and confirms its writer in Go against the client's reading of it would
either come back empty or find the third one. That is a bounded question with a
verifiable answer, which is more than "read `SubAgentBlock` again" ever was.
