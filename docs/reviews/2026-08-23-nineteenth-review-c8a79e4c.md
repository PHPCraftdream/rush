# Nineteenth review — `bcb098a0..c8a79e4c`

**Scope:** one commit (`c8a79e4c`, comment-only) plus an explicitly broad sweep
across areas this session's eighteen prior rounds have not reviewed in their own
right.

**Out of scope by instruction:** `internal/server/handlers_agent.go`'s
`handleRerunMessage` / `recreateRerunPromptIfLost` mechanism. Closed by the
eighteenth review; not re-opened, not attacked, nothing filed against it.

**Date:** 2026-08-23
**Reviewed at:** `c8a79e4c` (working tree clean apart from the known
` D web/dist/.gitkeep` and untracked `docs/`).

---

## Verdict

**NO-GO on "nothing to file" — GO on shipping.**

There are **no P1/P2/P3 defects**. `c8a79e4c` is accurate in every claim it
makes, and I verified each one independently rather than trusting the commit
message. Nothing found in the sweep threatens correctness, data integrity, or
concurrency.

But this round does **not** come back empty, and I am not going to manufacture
an empty report to end the loop. Four Minor findings are filed. Three of them
are doc/behaviour divergences of exactly the class this series has been closing
for five rounds; one (**M-3**) is a real, reachable *behavioural* divergence in
the web UI — the first non-comment finding in four rounds — with a mechanism I
can name end to end.

The honest summary for the orchestrator: **the backend is done.** I spent the
bulk of this round in `internal/server`, `internal/session`, `internal/db`,
`internal/agent/coordinator*`, `internal/agent/agent_compaction.go`, and
`internal/shell` and found nothing worth filing in any of them — every hazard I
went looking for (goroutine leaks, unsynchronised reads, torn commits, missing
release paths, unbounded buffers, TOCTOU on the lock files) is already closed and
already documented, usually with the reasoning for *why* the obvious alternative
was rejected. What is left is (a) a rename that finished in the code and never
finished in the comments, and (b) the frontend, which nineteen rounds have barely
touched.

If the exit condition is "a review that finds nothing anywhere in the
repository," this is not it. If the exit condition is "a review that finds
nothing that can hurt an operator's data or a running session," it is.

---

## §1 — Independent verification of `c8a79e4c`

Every location claim re-derived from current code at HEAD, not from the commit
message. **All accurate; no line number has drifted.**

| Edited comment | Claim | Verified at HEAD |
|---|---|---|
| `cliprovider/usage.go:17` | `agent_usage.go:88` — `PromptTokens = InputTokens + CacheReadTokens + CacheCreationTokens` | ✅ `agent_usage.go:88` is literally `promptTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens`. Line number still exact. |
| `cliprovider/usage.go:18` | `agent_usage.go:45-48` — cost = `in*Input + inCached*CacheCreation + outCached*CacheRead + out*Output` | ✅ `:45` `CostPer1MInCached…CacheCreationTokens`, `:46` `CostPer1MOutCached…CacheReadTokens`, `:47` `CostPer1MIn…InputTokens`, `:48` `CostPer1MOut…OutputTokens`. All four terms present, range exact. |
| `cliprovider/effort.go:10` | field is `session.SmartModelReasoningEffort`, a persisted column | ✅ `internal/session/session.go:62`; DB column `smart_model_reasoning_effort` (`internal/db/models.go:113`). |
| `cliprovider/effort.go:11` | `agent_turn.go:386` puts it on the context for every model, no per-provider guard | ✅ `agent_turn.go:386` = `ctx = context.WithValue(ctx, cliprovider.ReasoningEffortContextKey, currentSession.SmartModelReasoningEffort)`, unconditional in `runTurn`'s preamble. Line number exact. |
| `tools/ask_question.go:57` | the error-classification chain is in `agent_turn.go`, not fantasy's `agent.go` | ✅ `agent_turn.go:232-250` — the paragraph explaining the fantasy `executeSingleTool` propagation, then `var askErr *tools.AskQuestionError; if errors.As(err, &askErr)` at `:245-246`, normalising to `AwaitingAnswerError`. The fork's own file, as claimed. |
| `agent_turn.go:110-119` | `resolveTurnConfig` builds an immutable per-call snapshot (`agent.go:302-308`); `runTurn` reads `smartModel := cfg.smartModel` and passes it in rather than re-reading `a.smartModel` at fire time | ✅ `resolveTurnConfig` declared `agent.go:302`, `smartModel: a.smartModel.Get()` at `:304`, call-pin override at `:309-311`. `smartModel := cfg.smartModel` at `agent_turn.go:277`. **And the load-bearing half:** the onFire dispatch at `agent_turn.go:476` is `a.handleWatchdogFire(cause, elapsed, call.SessionID, &watchdogCauseVal, toolMaxDuration, idleTimeout, smartModel)` — the runTurn local, not `a.smartModel.Get()`. The rewritten paragraph's central claim is true of the wiring, not just of the prose. |
| `main.go` | routes are `/auth`, `/auth/check`, `/ws`, `/`; TCP, not Unix socket/named pipe | ✅ `server.go:116` `/auth`, `:117` `/auth/check`, `:120` `/ws` (behind `auth.Middleware`), `:124`/`:127` `/` (embedded SPA, or dev-server redirect when `static == nil`). `server.go:95` `lc.Listen(ctx, "tcp", s.addr)`. |
| `main.go` | no LSP, no workspaces, no `/v1` | ✅ No `internal/lsp` or `internal/workspace` directory exists. `grep -rn '"/v1' --include=*.go internal/ main.go` → no match. |
| commit's scope claim | comment-only, no logic change | ✅ Re-read the full diff: 5 files, every changed line inside a `//` comment. |

**Also independently checked and confirmed** the substantive half of the
`usage.go` fix — that the *old* formula was not merely stale but wrong:
`agent_usage.go:69-83` documents the two-term sum as a fixed bug (measured turn
`input=5842 / cache_creation=16984 / cache_read=0`, true prompt 22826, recorded
as 5842, 74% understatement, delaying compaction), and `agent_usage.go:70-71`
cites `cliprovider/usage.go` as its own authority for the correct disjointness
claim. The two files did contradict each other before this commit; they agree
now.

`go build ./...` clean at HEAD, `gofmt -l internal/ main.go` empty. The known
pre-existing `internal/csync/maps.go:148` vet warning is not re-filed.

---

## §2 — Findings, by severity

### P1 / P2 / P3 — none.

---

### M-1 (Minor) — the Large→Smart / Small→Fast rename finished in the code and never finished in the comments: 18 sites across 12 files, one of them user-facing `--help` text, one of them a Go doc comment naming a function that does not exist

`c8a79e4c` fixed exactly one member of this family —
`session.LargeModelReasoningEffort` → `session.SmartModelReasoningEffort` in
`cliprovider/effort.go:10` — treating it as a one-off. It is not a one-off. The
same rename left **18 other comment sites** naming struct fields, function
parameters, local variables, and one function *name* that no longer exist
anywhere in the tree.

This is structurally identical to the seventeenth review's M-3 ("`f4ab5b6b`
fixed one stale `(agent.go)` pointer and left three identical ones in the other
file it edited") and to the eighteenth's M-1, just on a different axis: the
prior rounds mined *file pointers*, this one is *identifier names*.

**Complete enumeration** (each verified against the declaration it purports to
describe):

| Site | Comment says | Actual declared name |
|---|---|---|
| `internal/agent/agent.go:229` | "LargeModel/SmallModel/SystemPromptPrefix, when set, pin this call's…" | `SmartModel *Model` / `FastModel *Model` — declared **16 lines below, at `:245-246`**, in the very block the paragraph introduces |
| `internal/agent/agent.go:233` | "The agent's largeModel/smallModel/systemPromptPrefix are process-wide" | `a.smartModel` / `a.fastModel` / `a.systemPromptPrefix` |
| `internal/agent/call_data_conversion.go:112` | "LargeModel and SmallModel are NOT set here" | `SmartModel` / `FastModel` |
| `internal/agent/coordinator_interrupt.go:470` | "it does NOT recompute them from LargeModel itself" | `SmartModel` |
| `internal/agent/coordinator_models.go:134` | "pinned onto every call's SmallModel (title generation …)" | `FastModel` |
| `internal/agent/coordinator_models.go:192` | "the SAME pinned cfg used for largeModel/largeProviderCfg above" | locals are `smartModel` (`:154`) and `smartProviderCfg` (`:176`) |
| `internal/agent/coordinator_models.go:216` | "for runSubAgent's per-call LargeModel pin (task #466)" | `SessionAgentCall.SmartModel` |
| `internal/agent/coordinator_models.go:333` | "the SAME pinned cfg used for largeModel above" | local is `smartModel` |
| `internal/agent/coordinator_run.go:201` | "the agent re-reads its shared largeModel when the turn actually starts" | `a.smartModel` |
| `internal/agent/coordinator_run.go:224` | "pin() leaves LargeModel as set above when pinned is nil" | `SmartModel` (set at `:220`, four lines above the comment) |
| `internal/agent/coordinator_subagents.go:119` | "the same immutable per-call pin SessionAgentCall.LargeModel already uses" | `SessionAgentCall.SmartModel` |
| `internal/agent/coordinator_summarize.go:24` | "specialized for summarize which doesn't need smallModel or systemPrompt" | `fastModel` |
| `internal/app/app_agent_setup.go:28` | "If largeModel is provided but smallModel is not…" | parameters are `smartModel, fastModel string` — **declared on the next line, `:30`** |
| `internal/app/app_agent_setup.go:76` | "**GetDefaultSmallModel** returns the default fast model…" | the function is `GetDefaultFastModel` (`:78`). A Go doc comment whose first word is a name that does not exist |
| `internal/cmd/sessions_cost.go:23` | `crush sessions cost --help`: "`model — group by LargeModelID (default)`" | `costByModel` groups by `s.SmartModelID` (`:172`). **User-facing.** |
| `internal/server/handlers_models.go:28` | "p.LargeModel/p.SmallModel being nil means…" | `p.SmartModel` (`:34`) / `p.FastModel` (`:39`), six lines below |
| `internal/server/handlers_sessions.go:40` | "A session is created with no override (LargeModelID/SmallModelID == \"\")" | `SmartModelID` / `FastModelID` |
| `internal/server/protocol.go:100` | "same convention as LargeModel/SmallModel above" | `SmartModel` / `FastModel`, `:97-98` — literally the two lines above |

**Deliberately excluded as NOT stale** (checked, they name real current
identifiers of an external type): `p.DefaultLargeModelID` /
`p.DefaultSmallModelID` in `internal/config/load_providers.go:498,501,503,514,517,519`
and `knownProvider.DefaultSmallModelID` in `internal/app/app_agent_setup.go:98`
— these are genuine fields of `catwalk.Provider`
(`charm.land/catwalk@v0.28.1/pkg/catwalk/provider.go:58-59`). Renaming them in
comments would make the comments wrong.

**Judgment call, not counted:** `coordinator_models.go:125-131` uses
`largeCfg`/`smallCfg`/`ModelBuiltFromLargeCfg` inside a narrative about code that
was *deleted*. Historical prose describing a removed shape is defensible; I list
it so whoever fixes M-1 makes the call deliberately rather than by accident.

**Why this survived nineteen rounds:** it is not lint-detectable in this repo.
`revive` — whose `exported` rule is the one that would flag
`app_agent_setup.go:76` mechanically — is commented out in `.golangci.yml:19`.
Nothing in CI can see any of these.

**Severity:** Minor. Zero runtime effect. The two worst instances are
`sessions_cost.go:23` (an operator reading `--help` is told the grouping key is a
field that does not exist) and `agent.go:229` (a paragraph that exists solely to
explain the pinning contract, misnaming the two fields it is explaining, sixteen
lines above their declaration — precisely the "comment actively misleads a reader
of this code" shape the earlier rounds filed).

---

### M-2 (Minor) — `crush sessions gc --help` advertises a deletion rule the code does not implement, and an in-body comment asserts that it does

`internal/cmd/sessions_gc.go:20-21`, in the `Long` help string printed by
`crush sessions gc --help`:

```
  1. Sessions older than --older-than (default 7 days) with zero messages
     or only system messages.
```

`classifyForGC` (`sessions_gc.go:147-166`) implements only the first half:

```go
// Rule 1: old sessions with zero messages.
if age > olderThan && s.MessageCount == 0 {
    return "empty session older than threshold"
}
```

There is no role inspection anywhere in the file — `classifyForGC` never loads
messages and never sees a `Role`.

The compounding part is `sessions_gc.go:109-111`, a comment in the middle of
`sessionsGcCmdRun`:

```go
// Collect messages to determine if a session has only system messages.
// We only need this for the "empty-or-system-only" classification which
// is already done in classifyForGC via MessageCount == 0.
```

That assertion is **false by construction**, not merely by omission.
`message_count` is maintained by two unconditional SQL triggers
(`internal/db/migrations/20250424200609_initial.sql:68-81`,
`message_count = message_count ± 1` on every insert/delete) with no role filter,
so a session holding N system messages has `MessageCount == N > 0` and can never
satisfy `MessageCount == 0`. The field cannot express the advertised condition at
all. The comment is not describing an approximation; it is describing something
the data model makes impossible.

**Direction of error is safe** — the command under-deletes relative to its
advertised behaviour, never over-deletes. That is why this is Minor and not
higher: an operator who trusts the help text loses nothing except sessions that
stay on disk. But `gc` is a destructive command, and a destructive command whose
`--help` and whose own internal comment both describe a rule it does not run is
exactly the kind of divergence that becomes dangerous the moment someone "fixes"
the code to match the docs without checking which side is authoritative.

**Fix is a one-line choice**, and the choice matters:
- either drop "or only system messages" from `:21` and rewrite `:109-111` to say
  the classification is `MessageCount == 0` and deliberately does not consider
  roles, **or**
- implement it (a `messages.List` per candidate, only for sessions past the age
  threshold, checking every message has `Role == system`).

The first is almost certainly right — the second costs one query per candidate
session for a case that essentially never occurs (nothing in the fork creates a
session containing only system-role messages).

---

### M-3 (Minor, behavioural) — the sub-agent transcript implements none of the three message-lifecycle rules the main transcript implements; all three become visible the first time a sub-agent session compacts

This is the round's only non-comment finding, and it is real. The web UI has two
renderers for agent messages, and they have silently diverged.

**The main renderer** (`web/src/components/Message/Message.tsx:35-36`):

```tsx
if (message.Hidden) return null;
if (message.IsSummaryMessage) return <SummaryMessage message={message} />;
```

plus deletion, handled in `web/src/useWS.ts:200-206` → `removeMessage`.

**The sub-agent renderer** (`web/src/components/SubAgentBlock.tsx:129-133`):

```tsx
{messages
  .filter((m) => m.Role === "assistant")
  .map((m) => <SubAgentMessage key={m.ID} message={m} />)}
```

No `Hidden` guard. No `IsSummaryMessage` case. And no delete path exists at all:
`useWS.ts`'s `message_created` (`:174-186`) and `message_updated` (`:187-199`)
handlers both route sub-agent sessions to `upsertSubAgentMessage`, but
`message_deleted` (`:200-206`) does not:

```ts
ws.on("message_deleted", (msg: WSMessage) => {
  const m = msg.payload as Message;
  const activeID = $activeSessionID.get();
  if (!activeID || m.SessionID !== activeID) return;   // ← sub-agent sessions exit here
  removeMessage(m.ID);
}),
```

`grep -rn "removeSubAgentMessage" web/src/` → **no match**. The function does not
exist; `store.ts` has `upsertSubAgentMessage` and `setSubAgentMessages` and
nothing else.

**Mechanism, end to end:**

1. `internal/server/events.go:88` broadcasts `EventMessageDeleted` for **every**
   deleted message, with no session filter — the same subscription that feeds
   `message_created`/`message_updated`. Sub-agent sessions are not special-cased
   anywhere in that goroutine.
2. Sub-agent sessions compact. Nothing gates either compaction path on
   `isSubAgent`: `agent_turn.go:1049` (`if !a.disableAutoSummarize`, the sliding
   window → `silentCompactNeeded = true` at `:1070`) and `agent_turn.go:1540`
   (`remaining <= threshold && !a.disableAutoSummarize` → `shouldSummarize`). I
   grepped every `isSubAgent` use in `internal/agent/*.go`; the only behavioural
   gates are the system-prompt orchestrator block (`agent_prompt.go:95`), the
   watchdog-ownership rule (`agent_timeouts.go:69`), hook wrapping
   (`hooked_tool.go:32`), and tool/model layering. Compaction is not among them.
3. Both compaction bodies delete the messages they replace —
   `agent_compaction.go:507-511` (`runSummarizeBody`) and `:690-694`
   (`runSummarizeSilent`) — and each `a.messages.Delete` publishes the
   `DeletedEvent` that step 1 fans out.
4. Both also create a summary message the sub-agent renderer has no case for:
   `runSummarizeBody` creates `IsSummaryMessage: true`
   (`agent_compaction.go:369-375`); `runSummarizeSilent` creates
   `IsSummaryMessage: true, Hidden: true` (`:563-570`).

**Observable result, for any sub-agent whose delegated task runs long enough to
compact:**

- Deleted messages stay in the block forever. `$subAgentMessages` is only ever
  replaced wholesale by `setSubAgentMessages`, which fires on a `messages_list`
  reply — and `SubAgentBlock`'s lazy load (`SubAgentBlock.tsx:91-97`) is guarded
  by `if (messages.length > 0) return;` plus a `requested` ref, so once the store
  holds stale messages nothing re-fetches for that sub-session until a page
  reload. The staleness is permanent for the tab's lifetime.
- The compaction summary is rendered inline as ordinary agent prose. For the
  `runSummarizeSilent` case that message is explicitly `Hidden: true` — the main
  chat drops it on the floor; the sub-agent block prints a full conversation
  summary as if the sub-agent had written it as its answer.

**Severity:** Minor. No data loss, no backend effect, entirely a rendering
divergence — but it is a rendering divergence the operator will read as the
sub-agent's own output, which is worse than a visibly-broken block.

**Fix shape:** add `removeSubAgentMessage(sessionID, id)` to `store.ts`, route
`message_deleted` through `isSubAgentSession` exactly as its two siblings already
do, and give `SubAgentMessage` the same two guards `Message.tsx:35-36` has. Four
small edits, all in `web/`.

**Not filed, related:** `SubAgentBlock.tsx:91` — the `requested` ref is never
reset when `subSessionID` changes, and `ToolActivityGroup.tsx:197` keys the
component by array index (`key={`a-${idx}`}`). A reused instance whose
`subSessionID` prop changes would never issue its lazy load. I could not
construct a case where that actually bites (live sub-agents are populated by the
`session_created` handler regardless, and after a reload the parts array is
static so index→toolCall is stable), so it stays an observation.

---

### M-4 (Minor, dev-scoped today) — `ws.ts`'s `onclose` mutates shared client state without checking it is still the current socket

`web/src/ws.ts:38-47`:

```ts
sock.onclose = () => {
  this.socket = null;                       // ← unconditional
  this.emit("_disconnected", { type: "_disconnected" });
  if (!this.closed) {
    this.reconnectTimer = setTimeout(() => { … this._connect(); }, this.reconnectDelay);
  }
};
```

`onclose` is a closure over `sock`, but it writes `this.socket` and fires
`_disconnected` without asking whether `sock` is still the socket the client
considers current. `disconnect()` (`:54-62`) has the mirror gap: it calls
`this.socket?.close()` — which is **asynchronous**, the handler fires on a later
task — and then drops the reference without detaching `sock.onclose`.

**Reproducing sequence** (`disconnect()` followed by `connect()`):

1. `_connect()` → socket **A**, `this.socket = A`.
2. `disconnect()` → `closed = true`, timer cleared, `A.close()` **queued**,
   `this.socket = null`.
3. `connect()` → `closed = false`, `_connect()` → socket **B**,
   `this.socket = B`.
4. A's close completes → **A's** `onclose` fires → `this.socket = null`, which
   **orphans B** (still open, still delivering into `emit`, unreachable by
   `send()` since that early-returns on `!this.socket`) → `_disconnected` fires
   spuriously → `closed` is now `false`, so a reconnect is scheduled → socket
   **C**.

Net: two live sockets both feeding `emit`, so every `message_updated` runs
`trackMessageParts` twice and every assistant `message_created` fires
`track_model_usage` twice; plus a phantom "Reconnecting…" banner
(`App.tsx:15-19`) and a keep-alive stop/start cycle (`useWS.ts:90-98`).

**Reachability, stated honestly:** today this is **development-only**.
`main.tsx:13` wraps the app in `<StrictMode>`, whose dev-mode
mount→cleanup→mount of `useWS`'s effect (`useWS.ts:47` / `:360`) is exactly the
sequence above. In production the sequence needs `AuthedApp` to unmount and
remount, which needs `$authed` to go back to `false` — and `$authed` is only ever
`.set(true)` (`App.tsx:33`, `Login.tsx:30`), never false. So there is no
production path today.

I am filing it anyway because the *missing guard is unconditional*: nothing in
`ws.ts` records which socket a given handler belongs to, so the safety of the
whole reconnect path rests entirely on the accident that `$authed` is
write-once. The fix is two lines and removes the dependency on that accident:

```ts
sock.onclose = () => {
  if (this.socket !== sock) return;   // a superseded socket must not touch client state
  …
};
```
plus `sock.onclose = null` (or the same identity check) before
`this.socket?.close()` in `disconnect()`.

**Severity:** Minor, and lower than M-3 — nothing an operator sees in a shipped
build. It is a latent correctness gap and a daily annoyance for anyone running
`pnpm dev`.

---

## §3 — Swept clean (no finding)

Each of these was read end to end with a specific hazard in mind, and the hazard
is closed. Listed so the next round does not re-spend budget here.

- **`internal/server/hub.go` — the worker-pool goroutine leak.** `startWorkers`
  spawns `workerPoolSize` (12) goroutines per `Client` that `range c.workQueue`,
  a channel `newClient` creates and nothing in `hub.go` closes — the textbook
  12-goroutines-per-connection leak. It is closed: `readPump`'s
  `defer close(c.workQueue)` (`server.go:203`) is safe *specifically* because
  `dispatch` is only ever called synchronously from `readPump`'s own read loop,
  so by the time the defer runs no producer can still be sending. The defer
  ordering (LIFO: `close(workQueue)` → `unregister` send → `conn.Close()`) is
  also right, and the `unregister` send is `ctx.Done()`-guarded so a full
  channel during shutdown cannot strand the goroutine.
- **`internal/server/hub.go` — sticky-vs-replay ordering.** The
  "never a superseded copy last" invariant holds for the stated reason: sticky
  envelopes are read from `h.sticky` at drain time and never enter
  `h.buffer`, so no older generation exists for the replay to deliver after the
  current one. Dropped wakeup tokens are genuinely harmless (the pending *set*,
  not the envelope, is what the token signals).
- **`internal/server/handlers.go` — the dispatch table.** All 47 commands
  present; exactly two (`CmdCancelAgent`, `CmdInterruptAndSend`) take
  `dispatchControl`, and both are genuinely fast (in-memory map lookup +
  `CancelFunc`, or a bounded DB read) as the comment claims. Both admission
  paths are non-blocking, so `readPump` is never the backpressure point (#612).
- **`internal/server/auth.go` / `checkOrigin`.** Constant-time compare with an
  explicit length pre-check; `s.port` is written in `Start` before `srv.Serve`
  spawns any handler goroutine, so `checkOrigin`'s read is properly
  happens-after. Empty-Origin acceptance is correct for the fork's
  non-interactive use case and reasoned about in the comment.
- **`internal/db/connect.go`.** Per-path `sync.Map` mutex (so unrelated
  `dataDir`s don't serialise behind one migration), `poolMu` never held across
  open/ping/migrate, every failure path closes `conn`, `closeEntry` tolerates
  `readDB == db`, `ReleaseAll` zeroes `refCount` before unlinking. The only
  unbounded thing is `pathLocks` growing one mutex per distinct path forever —
  bounded in practice by the number of data directories a process ever opens.
- **`internal/agent/agent_compaction.go`** (read end to end, first time this
  session). Both commit phases are ordered summary-pointer-then-delete and both
  run on a bounded `context.WithoutCancel` (`summaryCommitMaxDuration`), so
  neither can be torn into a holed history. `runSummarize`'s ownership handling
  releases the mailbox on every error branch (`abandonOwnershipWithHandoff` on
  lock-busy, lock-error, and body-error) and uses the atomic
  `abandonOwnershipAndPopFirstSubmitted` on success. `getSessionMessages`
  (`agent_prompt.go:263-293`) preserves pinned pre-summary messages and applies
  `msgs[0].Role = message.User` **before** prepending them, which is the correct
  order.
- **`internal/agent/coordinator_run.go`.** The retry loop's one-shot fields
  (`allowPeakHours`, `maxCost`, `maxTokens`) are consumed once and baked into
  the call, so a retry cannot re-consume them; `LogicalCallID` is deliberately
  stable across retries; the interrupt-ticker defer combines
  `stopTicker()` + `<-tickerDone` in one func so the join order cannot be
  broken by a later-inserted defer. `RunWithReservedOwnership`'s single-handoff
  discipline holds: all three early returns above the handoff line call
  `ReleaseExclusive`, nothing below it does.
- **`internal/agent/coordinator_tools.go`.** The one-snapshot-per-build rule
  (task #576/P1-3) is actually honoured — models, prompt, hooks, tool options
  and the worker/orchestrator tool layering all read the same pinned `cfg`; the
  two documented live reads (`PeakHoursCheck`, MCP registry) are the only
  exceptions and both carry their justification.
- **`internal/shell/background.go`.** `bgShell.exitErr` is written before
  `defer close(bgShell.done)` runs, so every reader that goes through
  `<-bs.done` is properly synchronised. `activeJobs` is decremented in the one
  place all three termination routes (normal exit, `Kill`, `KillAll`) converge.
  `releaseBuffers` is CAS-idempotent and the `time.AfterFunc` callback carries
  its own recover.
- **`internal/session/lock.go`.** `clearHolderMetadata`'s residual TOCTOU is
  documented, bounded, and explicitly accepted with the history of *why* the
  two obvious alternatives were reverted. The heartbeat's activity gate, the
  "missing generation sidecar is NOT evidence of a new owner" rule, and the
  strict separation of diagnostics from the OS lock as source of truth are all
  consistent between doc and code.
- **`internal/server/handlers_models.go`.** The reasoning-effort preservation
  (`:95-109`) genuinely closes the "one arrow click clobbers the other slot"
  hazard by re-reading the unset side from the DB, and
  `store.ts:setSessionReasoningEffort` sending both slots is safe against it.
  `scopedModelsScopeFromWire`'s stricter-than-sibling error on an unknown scope
  is right.

---

## §4 — Observations (no finding)

- **`coordinator_tools.go:46-82` — a nil-guard that guards nothing.** `opts :=
  cfg.Options` is nil-checked three times (`:48`, `:53`, `:58`,
  `if opts != nil && …`) and then dereferenced unguarded at `:74`
  (`opts.DisableAutoSummarize`) and `:82` (`opts.DataDirectory`), which run
  *after* all three checks. If `opts` could be nil, the guards would buy nothing
  — the function would panic eleven lines later. This is the same inconsistency
  `handlers.go:166-175` already fixed in the other direction, and its comment
  there is the authority for why it is not a live crash ("`setDefaults`
  (config/load.go) always installs an `&Options{}`"). Worth collapsing to one
  convention next time someone is in the file; not worth a commit of its own.
- **`agent_compaction.go:498` vs `:681` — asymmetric completion-token
  fallback.** `runSummarizeBody` records
  `summaryCompletionTokens(usage, summaryMessage)` (which estimates from the
  rendered text when the provider reports zero); `runSummarizeSilent` records
  the raw `resp.Response.Usage.OutputTokens` with no fallback. Deliberate or
  not, the helper's own doc ("used in Summarize when the provider omits final
  usage") does not mention the silent path. Errs in the safe direction — a zero
  there slightly *over*-estimates remaining context, delaying nothing that
  matters.
- **`handleCreateSession` never replies to `msg.ID`** (`handlers_sessions.go:18-64`
  broadcasts `EventSessionCreated` and returns). Harmless today because the only
  caller, `useWS.ts:148`, sends `create_session` with no correlation ID — but it
  is the one handler in the table that silently drops a correlation ID a client
  might supply.

---

## §5 — Verification performed

- Full diff of `c8a79e4c` re-read; all five files confirmed comment-only.
- Every location claim in `c8a79e4c` re-derived from HEAD (§1 table). No drift.
- `go build ./...` clean; `gofmt -l internal/ main.go` empty.
- `git status` confirms the working tree carries only the pre-existing
  ` D web/dist/.gitkeep` and untracked `docs/` files. **No production code was
  changed by this review.**
- Route list cross-checked against `internal/server/server.go:116-128`;
  transport against `:95`; absence of `internal/lsp`, `internal/workspace` and
  any `/v1` route confirmed by directory listing and grep.
- `catwalk.Provider.DefaultLargeModelID` / `DefaultSmallModelID` confirmed as
  genuine external fields (`charm.land/catwalk@v0.28.1/pkg/catwalk/provider.go:58-59`)
  so they could be excluded from M-1 rather than mis-filed.
- `message_count` trigger semantics confirmed from
  `internal/db/migrations/20250424200609_initial.sql:68-81` for M-2.
- Sub-agent compaction reachability confirmed by enumerating every `isSubAgent`
  use in `internal/agent/*.go` and checking none of them gates
  `shouldSummarize` / `silentCompactNeeded`.
- `revive` confirmed disabled in `.golangci.yml:19`, explaining why M-1's
  strongest instance is invisible to CI.
- `staticcheck` was attempted as an independent cross-check and **could not be
  used**: the installed v0.4.7 panics
  (`ir.memberFromObject`, nil deref) against this Go version. Reported here
  rather than silently omitted — this round's static coverage is
  `go build` + `gofmt` + reading, not a third-party analyser.

---

## §6 — Things I could not verify, labelled as such

- **M-3 was not reproduced in a browser.** The web test harness is Playwright
  e2e (`web/tests/`, driven by `scripts/run-tests.mjs`), which needs a live
  server and a real browser — outside a read-only review's budget and outside
  the "test only what you touched" rule. The finding rests on code inspection,
  but every link in the chain is a cited line, and the two decisive negatives
  (`grep -rn "removeSubAgentMessage" web/src/` → no match; no `Hidden` /
  `IsSummaryMessage` guard in `SubAgentBlock.tsx`) are absolute rather than
  inferential.
- **M-4's dev-mode reproduction is reasoned, not executed.** React StrictMode's
  dev double-invocation of effects is documented behaviour and `main.tsx:13`
  does enable it, but I did not run `pnpm dev` to watch two sockets open.
- **No dynamic/behavioural testing was run at all this round** — no `crush run`,
  no server start, no CLI exercising. Per CLAUDE.md's warning about
  `CRUSH_GLOBAL_DATA`/`CRUSH_GLOBAL_CONFIG`, exercising `crush sessions cost` or
  `crush sessions gc --help` "in isolation" is not free, and neither M-1 nor M-2
  needs execution to establish — both are textual divergences between a string
  in the binary and the code beside it.

---

## §7 — What the orchestrator should do with this

1. **M-1 and M-2 are comment/help-text only**, no test implications, and can go
   in one commit. M-2 requires one decision (drop the advertised clause vs.
   implement it) — the drop is almost certainly right and should be stated in
   the commit message either way.
2. **M-3 is the one that changes behaviour** and is the only finding worth a
   test. It is four small edits in `web/` (`store.ts`: add
   `removeSubAgentMessage`; `useWS.ts`: route `message_deleted` through
   `isSubAgentSession`; `SubAgentBlock.tsx`: add the `Hidden` and
   `IsSummaryMessage` guards). A Playwright case is possible but expensive; a
   reviewer-verifiable alternative is to assert the three handlers in `useWS.ts`
   are structurally symmetric.
3. **M-4 is optional.** Two lines, no production impact today. Worth taking
   because it removes a dependency on `$authed` being write-once, which nothing
   documents or enforces.
4. **The backend should be considered closed for this review series**, on the
   same standard the eighteenth review applied to the rerun mechanism. Nineteen
   rounds have not left a reachable correctness defect in
   `internal/server`, `internal/session`, `internal/db`, `internal/agent`, or
   `internal/shell`, and this round went looking specifically in the parts those
   rounds had not opened. Further backend rounds should be triggered by a
   behavioural report from actual use, not by another read.
5. **If the loop continues, point it at `web/`.** M-3 and M-4 both came from
   there, in a single pass over five files, after eighteen rounds that barely
   touched it. That is where the remaining density is.
