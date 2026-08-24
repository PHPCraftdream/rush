# Twentieth review — `c8a79e4c..9c1d661b`

**Scope:** independent verification of this round's three commits (`7aeda794`,
`c5177703`, `9c1d661b`), then a fresh-eyes sweep of `web/` — per the nineteenth
review's own closing recommendation ("If the loop continues, point it at
`web/`… that is where the remaining density is").

**Out of scope by instruction, and honoured:**
- `internal/server/handlers_agent.go`'s `handleRerunMessage` /
  `recreateRerunPromptIfLost`. Not opened, not attacked, nothing filed.
- A fresh backend sweep. Backend was read only where a `web/` question required
  knowing what the server actually puts on the wire (`wire.go`, `events.go`,
  `handlers_messages.go`, `handlers_models.go`, `agent_turn.go`,
  `agent_compaction.go`, `message/content.go`). No backend finding is filed.

**Date:** 2026-08-24
**Reviewed at:** `9c1d661b` (working tree: the known ` D web/dist/.gitkeep` plus
untracked `docs/`; nothing else).

---

## Verdict

**GO on shipping. NO-GO on "nothing to file."**

There are **no P1/P2/P3 defects**. All three commits under review do what they
claim; I re-derived every claim from current code rather than trusting the
commit messages, and I reproduced `7aeda794`'s behavioural claim by execution
rather than accepting the worker's transcript.

The round does not come back empty. **Two Minor findings**, both in
`web/src/components/SubAgentBlock.tsx`, and both of the exact class the
nineteenth review pointed the loop at:

- **M-1 (behavioural).** `SubAgentBlock`'s `done` predicate reports a
  *still-running* sub-agent as finished — deterministically, after its first
  turn-step — which collapses the block and shows a green "done" badge for the
  whole rest of the run. This is the same "SubAgentBlock does not use the main
  renderer's message-lifecycle rules" divergence that produced M-3 last round;
  `c5177703` fixed three of those rules and left this one. Demonstrated by
  execution.
- **M-2 (comment precision).** `c5177703` introduced a comment that is falsified
  by a change in the same commit.

M-1 is a real, reachable, user-visible defect and I am not going to suppress it
to end the loop. It is also small: one predicate, no data at risk, no backend
change.

---

## §1 — Independent verification of this round's three commits

### `7aeda794` — `fix(web): guard ws.ts's onclose against a superseded socket`

**Reproduced by execution, independently of the worker's harness.** I wrote my
own Node harness (in the OS temp dir, outside the repo — the working tree was
not touched) that loads the **real** `web/src/ws.ts` module file under Node 24's
native TypeScript stripping, against a fake `WebSocket` whose `close()` flips
`readyState` to `CLOSING` immediately and delivers the close *event* on a later
macrotask — which is what browsers actually do and is the entire premise of the
bug.

Run against `git show 7aeda794^:web/src/ws.ts` and against HEAD's version:

```
########## PRE-FIX ##########
[1] connect -> disconnect -> connect, then A's late close lands
  PASS  A is the live socket after connect()
  PASS  B created by second connect()
  PASS  no third socket was spawned by a spurious reconnect
  FAIL  no spurious _disconnected emitted
  FAIL  send() still reaches B (B not orphaned)
  PASS  A is CLOSED
  PASS  B is still OPEN
  PASS  one inbound frame on B fires handlers exactly once
[2] genuine drop of the active socket
  PASS  _disconnected emitted on a genuine drop
  FAIL  a reconnect socket was created
  PASS  reconnect socket reports _connected
  PASS  send() reaches the reconnected socket
[3] explicit disconnect() must stay disconnected
  PASS  no socket created after explicit disconnect()
10 passed, 3 failed

########## POST-FIX ##########
  … all 13 PASS …
13 passed, 0 failed
```

The three pre-fix failures are the bug, precisely:

- *spurious `_disconnected`* — A's late `onclose` fired even though A was no
  longer `this.socket`;
- *`send()` no longer reaches B* — that same handler set `this.socket = null`,
  orphaning the live socket B (still open, still feeding `emit`, unreachable by
  `send`);
- *scenario 2's "a reconnect socket was created"* fails because **two** reconnect
  timers were then pending at once (one from the spurious close, one from the
  genuine drop) and both fired — a direct downstream consequence of the first two.

Post-fix all thirteen pass, including the two that matter for *not over-fixing*:
a genuine drop of the active socket still emits `_disconnected` and still
reconnects, and an explicit `disconnect()` still stays disconnected.

Diff read in full; it is exactly the two-hunk shape the message describes
(`ws.ts:39` guard, `ws.ts:61-64` detach-then-close). `pnpm typecheck` clean at
HEAD.

**Commit-message claims spot-checked and confirmed:** `main.tsx:13` does wrap the
app in `<StrictMode>`, so the dev double-invoke path the message names is real;
`$authed` is set `true` at exactly `Login.tsx:31` and `App.tsx:33` and never set
`false`, so `AuthedApp` genuinely does not remount in production and the
production-unreachability claim holds.

**One residual, deliberately not filed:** `disconnect()` detaches `onclose` but
leaves `onmessage` attached, and `_connect()` overwrites `this.socket` without
closing the previous socket. A superseded socket in `CLOSING` state can still
deliver queued frames into `emit`. Reachable only through the same
StrictMode-only sequence the commit already closes for state mutation, and the
commit's stated goal was the state-mutation half. Noting it, not filing it.

### `c5177703` — `fix(web): give SubAgentBlock the same message-lifecycle rules as the main renderer`

Traced end to end against current code, at the rigour the nineteenth review used
to *find* M-3.

**Routing.** `useWS.ts:201-214`'s `message_deleted` now tests
`isSubAgentSession(m.SessionID)` before the active-session gate, mirroring
`message_created` (`:175-187`) and `message_updated` (`:188-200`) exactly —
same predicate, same early `return`, same position in the handler. Confirmed the
wire actually carries the field this depends on: `events.go:88` broadcasts
`EventMessageDeleted` with the raw `message.Message` (not `toMessageWire`), and
`message.Message.SessionID` (`content.go:143`) has no json tag, so it marshals
as `"SessionID"` — the field `isSubAgentSession` is handed. The pre-existing
`m.SessionID !== activeID` check on the same payload already depended on this,
and `web/tests/message-delete.spec.ts:149` asserts the shape.

**Render guards.** `SubAgentBlock.tsx:138-146` now filters `!m.Hidden` and routes
`m.IsSummaryMessage` through `SummaryMessage`, matching `Message.tsx:35-36`.
Verified against the Go source rather than the review summary: silent compaction
creates its summary with `IsSummaryMessage: true, Hidden: true`
(`agent_compaction.go:563-570`); visible compaction with `IsSummaryMessage: true`
and no `Hidden` (`:369-375`). **Both use `Role: message.Assistant`**, which
matters because `SubAgentBlock` filters `m.Role === "assistant"` *before* the
summary branch whereas `Message.tsx` checks `IsSummaryMessage` *before* looking
at role — with a non-assistant summary the two would still diverge. They don't.

**`SummaryMessage` inside `SubAgentBlock`'s narrower container** — read both.
`SummaryMessage` is self-contained (`.summary-card`, `index.css:369`) and has no
width, position or portal assumptions; it renders `px-8 py-3` inside
`.sub-agent-body`'s `px-5` (`index.css:363-366`), so the card is inset ~3.25rem
per side. Visually tight, structurally fine. Its Edit button calls
`updateMessageContent(message.ID, …)`, which is message-ID-scoped and works for a
sub-agent message identically. `useCollapseAllSignal` is global and unaffected by
nesting. No new problem introduced.

**Store.** `removeSubAgentMessage` (`store.ts:292-300`) mirrors
`upsertSubAgentMessage`'s copy-on-write shape, returns early on a missing session
*and* on a no-op filter (so it never publishes an identity change for nothing).
Correct.

**Keying / latch.** `ToolActivityGroup.tsx:197` now keys by `part.ID`; the
`requested` ref latches on `subSessionID` rather than a boolean. I checked the
new latch does not create a refetch loop: after `removeSubAgentMessage` empties a
slice, the effect re-runs with `messages.length === 0` but
`requested.current === subSessionID`, so it returns without re-sending. The one
case that *does* re-send once — a block that mounted with a pre-populated store
and was later emptied by compaction — is correct behaviour, not a loop.

`pnpm typecheck` clean at HEAD. No component-level test infrastructure exists for
this file class (confirmed: no Vitest/jest/`node:test` in `web/package.json`,
zero `*.test.*` under `web/src`; `web/tests/` is Playwright only) — consistent
with what the worker reported.

**The commit is correct in everything it changed.** M-1 and M-2 below are about
what it *left* and what it *added in a comment*.

### `9c1d661b` — the `Large`→`Smart` / `Small`→`Fast` comment rename + `sessions gc --help`

Fuller independent pass than a spot-check, as requested.

- **Every renamed identifier exists under its new name.** `SessionAgentCall.SmartModel`/`FastModel`
  at `agent.go:246-247`; the agent's private `smartModel`/`fastModel` at
  `agent.go:294-295` and `:425-426`; `GetDefaultFastModel` at
  `app_agent_setup.go:75`; `SetSessionModelsPayload.SmartModel`/`FastModel` at
  `protocol.go:96-97`.
- **Nothing stale was missed.** `grep -rn 'LargeModel|SmallModel|largeModel|smallModel|largeCfg|smallCfg' internal/ --include=*.go`
  (excluding tests) returns exactly eleven hits, and all eleven are the two
  categories the commit message names as deliberately left: catwalk's own
  `DefaultLargeModelID`/`DefaultSmallModelID` (`load_providers.go:498,501,503,514,517,519`,
  `app_agent_setup.go:98` — an external dependency's field names) and
  `coordinator_models.go:125,127,130,280`'s historical prose narrating deleted
  code. The commit message's exclusion list is exhaustive and accurate.
- **The user-facing claim.** `sessions_cost.go` `--help` now says `SmartModelID`;
  `costByModel` at `sessions_cost.go:172` is literally `model := s.SmartModelID`.
  Correct.
- **M-2 from last round.** `classifyForGC` (`sessions_gc.go:148-167`) has three
  rules and rule 1 is `age > olderThan && s.MessageCount == 0` — no role
  inspection anywhere in the file. The help text now says "with zero messages."
  and the replacement comment (`:108-112`) states the role-blindness plainly.
  Both accurate.

No logic changed. Nothing to file.

---

## §2 — Findings

### M-1 (Minor, behavioural) — `SubAgentBlock` reports a running sub-agent as "done" after its first turn-step

**File:** `web/src/components/SubAgentBlock.tsx`

The block's finished-state predicate is:

```ts
// :24-26
function isFinished(msg: Message): boolean {
  return msg.Parts?.some((p) => p.type === "finish") ?? false;
}
// :77-80
const done = useMemo(
  () => messages.some((m) => m.Role === "assistant" && isFinished(m)),
  [messages],
);
```

and it drives, at `:104-107`:

```ts
// Open while the sub-agent is still working (mirrors prior `open={!done}`
// behaviour); the user's manual toggle wins once they touch the chevron.
const [override, setOverride] = useState<boolean | undefined>(undefined);
const open = override ?? !done;
```

plus the green `done && <span …>done</span>` badge at `:126`.

The stated intent — "**while the sub-agent is still working**" — is not what the
predicate computes. `done` is true as soon as **any** assistant message in the
sub-session carries **any** finish part. Two independent ways that happens
mid-run:

**(A) Every turn-STEP gets a real, non-Partial finish.** `OnStepFinish`
(`internal/agent/agent_turn.go:1295`) runs after *each* step and reaches
`currentAssistant.AddFinish(finishReason, "", "")` at `:1380`, with
`finishReason` mapped to `message.FinishReasonToolUse` (`"tool_use"`) for a step
that ended in tool calls (`:1315-1322`). `PrepareStep` then reassigns
`currentAssistant` to a **new** message for step 2 — the fork's own architecture
note in `web/src/components/Chat.tsx:117-118` says it outright: "The agent emits
a brand-new assistant message per turn-step." So step 1's finished message stays
in `$subAgentMessages` next to step 2's live one, and `messages.some(…)` is true
for the rest of the run. **A sub-agent that makes even one tool call — i.e.
essentially every sub-agent — collapses and shows "done" after its first step.**

**(B) The auto-checkpoint ticker.** Every `checkpointInterval` (default `2s`,
`internal/agent/agent.go:166`; sub-agents get it too —
`coordinator_tools.go:56-83` passes `CheckpointInterval` for `isSubAgent` as
well) the ticker clones the live message, stamps a `Finish` with `Partial = true`
onto the clone (`agent_turn.go:799-805`) and persists+broadcasts it.
`PartWire.Partial` is on the wire (`internal/server/wire.go:38,116`) and
`FinishPart.Partial` is in `web/src/types.ts:92`. `isFinished` ignores it.

The main renderer's helper does not have either problem. It is per-message and
Partial-aware, with a doc comment explaining exactly why:

```ts
// web/src/components/Message/textParts.ts:23-25
export function isTerminallyFinished(parts: ContentPart[]) {
  return parts.some(p => p.type === "finish" && !p.Partial);
}
```

`Message.tsx:77` uses it. `SubAgentBlock` does not.

**Reproduction (executed).** Harness in the OS temp dir, outside the repo. It
imports the *real* `textParts.ts` and uses verbatim copies of
`SubAgentBlock.tsx:24-26` and `:77-80`, against part arrays in the exact wire
shape `toPartWire` emits:

```
case                                  SubAgentBlock.done   truth   collapsed?
A. step 1 done, step 2 streaming      true                 false   YES   <-- WRONG
B. single step, 2s checkpoint         true                 false   YES   <-- WRONG
   genuinely finished                 true                 true    YES

main renderer isTerminallyFinished(parts), per message:
  s1 ["thinking","tool_call","finish"] -> true
  s2 ["text"]                          -> false
  s2 ["text","finish"(Partial)]        -> false
  s3 ["text","finish"]                 -> true

2 case(s) where SubAgentBlock reports a running sub-agent as done
```

**Operator-visible effect.** From roughly two seconds into a sub-agent run
(case B) or from the end of its first tool step (case A, the dominant one):

1. the block's body **collapses**, hiding the sub-agent's live output for the
   remainder of the run;
2. the header renders the green **"done"** badge (`:126`) while simultaneously
   rendering the pulsing **"running…"** badge (`:125`) — `$busySessions` *does*
   carry sub-agent session IDs, refreshed by `handleListSessions`
   (`internal/server/handlers_sessions.go:230`) on the 5s `list_sessions` poll —
   so the two contradict each other on screen;
3. the `prevDone` effect (`:108-112`) fires `setOverride(undefined)` on the
   false→true edge, so an operator who had deliberately expanded the block has
   that choice discarded at the moment it collapses.

**Severity: Minor.** No data loss, no backend effect, no crash. But it hides
live output and asserts something false about run state, and it is the same
"two renderers of agent messages that silently diverged" shape as M-3 — in the
same component, filed one round after a commit whose title is "give
SubAgentBlock the same message-lifecycle rules as the main renderer."

**Fix shape (not applied — this is a review).** `done` needs to mean "this
sub-agent's run is over", which the current predicate cannot express from a
`some()` over all messages. The signals available to the component are
`busySessions.has(subSessionID)` (already read as `isRunning` at `:75`) and the
*last* assistant message's finish reason. Whatever is chosen, `isFinished` at
`:24-26` should either be replaced by the shared `isTerminallyFinished` from
`Message/textParts.ts` or deleted — a second hand-written copy of a lifecycle
predicate is exactly what M-3's fix reused `SummaryMessage` to avoid.

**Related, same root, deliberately not filed separately** — three other
ad-hoc `some(p => p.type === "finish")` copies that skip the `!p.Partial` guard.
All three are benign today and none is user-visible the way M-1 is; listing them
so a fix pass can sweep them together:

| Site | Use | Why it's benign |
|---|---|---|
| `Message/AssistantContent.tsx:38` | picks `FinishErrorBlock` vs "streaming…" | additionally gated on `!isLive` |
| `Message/SummaryMessage.tsx:20` | gates the Edit pencil + `DurationBadge` | a mid-stream edit is refused server-side (`updateMessageAndVerify`, `handlers_messages.go:70-78`) |
| `Message/DurationBadge.tsx:17` | stops the live elapsed timer | cosmetic; timer freezes and resumes |

**Also noticed while reading, trivial:** `SubAgentBlock.tsx:124` is
`{done ? "Agent" : "Agent"}` — a ternary with identical branches. Dead as
written; presumably the label was meant to differ by state. Fold into the same
fix.

### M-2 (Minor, comment precision) — a comment `c5177703` added is falsified by the same commit

**File:** `web/src/components/SubAgentBlock.tsx:92-95`

```ts
// Latch on the session ID rather than a bare boolean: ToolActivityGroup
// keys SubAgentBlock instances by parts-array index, so one instance can
// be handed a different subSessionID after a parts reorder. A boolean
// latch would then suppress the new session's lazy load forever.
```

`ToolActivityGroup` **does not** key by parts-array index. The same commit
changed it:

```
-      return <SubAgentBlock key={`a-${idx}`} …
+      return <SubAgentBlock key={`a-${part.ID}`} …
```

`ToolActivityGroup.tsx:197` at HEAD is `key={`a-${part.ID}`}`. The only other
render site, `Message/Part.tsx:26`, renders a single unkeyed element. So the
comment's premise — the mechanism it cites to justify the latch — is false as of
the commit that wrote it.

The latch itself is still worth keeping (a session-ID-valued latch is strictly
safer than a boolean regardless of keying, and `part.ID` is only stable while the
part exists), but the comment must not assert a keying scheme the codebase no
longer uses. The next reader who greps for `a-${idx}` finds nothing and has to
reconstruct which of the two statements is stale.

This is precisely the class M-1 and M-2 of the nineteenth review were — a
comment describing code that no longer exists — introduced one commit after that
class was supposedly swept clean.

**Fix:** restate the justification without the false premise, e.g. "a boolean
latch cannot distinguish 'already asked for *this* session' from 'already asked
for *some* session', so a reused instance handed a different `subSessionID`
would never load; latching on the value itself is correct independently of how
callers key us."

---

## §3 — Swept, nothing filed

Read in full and found clean, or clean enough that filing would be
manufacturing. Listed so the next round does not re-tread it.

**`web/src/ws.ts` end to end** — `_connect`, `send`, `on`, `emit`, reconnect
backoff (1s doubling to a 30s cap, reset on `onopen`), `disconnect`. `send`
correctly guards on `readyState !== OPEN`. `on` returns a working unsubscribe.
`emit` iterates a `Set` — a handler that unsubscribes during dispatch is safe
under `Set.forEach` semantics. Only unfiled note is the `onmessage` residual in
§1.

**`web/src/useWS.ts` end to end** — every handler, not just the two touched.
`_connected` correctly resets `$busySessions` before re-requesting state. The
reconnect path is covered by the server's replay ring (`hub.go:20`,
`maxBufferSize = 2000`), so a blip heals; a server *restart* leaves the
transcript stale until a session switch, but `sessions_list` re-polls every 5s
and `OwnedExternal` sessions re-fetch messages every 1.5s, and no data is lost —
not filed. The `messages_list` envelope handling correctly refuses to let an
empty sub-agent reply clobber the main session. Polling teardown on
`visibilitychange` and on unmount is complete (`stopPolling` clears both
intervals; the cleanup removes both DOM listeners, unsubscribes all handlers, and
disconnects, in that order).

**`web/src/store.ts` as a whole**, specifically hunting for more of M-3's
"two representations of the same data" shape. Found four places where a key
outlives its subject — `$busySessions`, `$subAgentSessions`, `$subAgentMessages`
and `$messageQueue` are never purged on `session_deleted`; `$messageBlockBreaks`
and the module-level `_msgPartTracker` are never purged on `message_deleted`.
All are UUID-keyed, so no collision is possible and no wrong value is ever read;
the cost is bounded memory in a long-lived tab. Not defects. The one with an
actual visible edge — `$selectedMessageIDs` keeping IDs of messages the *server*
deleted (compaction), leaving a phantom "N selected" toolbar — resolves on the
next session switch (`Chat.tsx:336-340`) and its only action is a delete of
non-existent IDs, which is a no-op. Not filed.

**`mergePreserveContent` / `upsertMessage`** — probed hard. It guards thinking
and text against a stale re-broadcast but not the terminal `finish` part, and
`events.go:83-85`'s per-ID batch is last-arrival-wins with no ordering
protection, so on paper a late stale snapshot could un-finish a message in the
UI. **It cannot**: `agent_turn.go:1383-1388` explicitly drains the pending
`latestMsgCh` snapshot before the terminal write, and
`message.Service.Update`'s partial-checkpoint branch uses
`UpdateMessageIfNotTerminal` and *does not publish* when it loses
(`message.go:394-418`). Both halves of the hazard are already closed on the
server. Not filed.

**Client-side part reordering vs. `partIndex`** — `mergePreserveContent`'s
rebuild path can hand the client a `Parts` order different from the DB's, while
`update_message_part` / `delete_message_part` apply a raw index against the DB
order (`handlers_messages.go:332-336`, `:354-358`). The rebuild only triggers
during streaming, and `updateMessageAndVerify` refuses every write to a
still-streaming assistant message (`:70-78`), so a mismatched index can never be
committed. Once the turn is terminal the client's order re-syncs (no regression →
`return incoming` verbatim). Not filed.

**`ThinkingPart`'s Edit → `update_message_thinking`** — that handler rewrites the
**first** `ReasoningContent` part and breaks (`handlers_messages.go:295-310`),
which would be wrong for a message with several. It cannot happen:
`Message.AppendReasoningContent` (`message/content.go:317-336`) appends the delta
to every existing reasoning part and only creates one when none exists, so a
message never has more than one. `store.ts:135-137`'s "reasoning can span
multiple parts" comment overstates it, but the defensive summing is harmless.
Not filed.

**Rules-of-Hooks in `Message.tsx`** — three early returns (`:35-37`) precede all
hooks, which would crash React if `Hidden` / `IsSummaryMessage` /
`BackgroundJobNotice` ever flipped on a live row. All three are write-once at
`message.Create` (`message/message.go:294-308`) and `Hidden` rows never reach the
component at all (`Chat.tsx:187` drops them upstream). Latent, unreachable. Not
filed.

**One-shot `ws.on` handlers never unsubscribed on unmount** — `LogsModal:19`,
`MCPSettings:121`, `ProvidersModal:160,394`, `SettingsModal:91,153`,
`ChatToolbar`'s `SystemPromptModal:39`. Each unsubscribes *inside* its own
handler after correlating on `msg.id`, so closing the modal before the reply
arrives leaves a handler registered forever (several of them wildcard `"*"`
handlers that then run on every streaming delta). Bounded by the number of
aborted requests; setState on an unmounted component is a no-op in React 18+.
`ScopedModelsModal:309-315` does it correctly (`return unsub`). A house-pattern
inconsistency, not a defect. Not filed.

**`installKeepAliveAutoResume`** (`keepAlive.ts:105-112`) adds a
`visibilitychange` listener that is never removed, and `useWS`'s cleanup does not
remove it. One mount in production; doubles under StrictMode in dev. Not filed.

**`TodoList` optimistic-edit race** — `TodoRow` keys by array index and holds
`draft` in local state; an agent `update_todos` landing while a row is open for
editing would commit the operator's text onto whatever todo now occupies that
index. Genuine but requires an unlucky concurrent write, and the blast radius is
one todo's text. Judged below the bar for this round; recording it so a future
round can pick it up with a concrete report.

**Also read, nothing found:** `App.tsx`, `main.tsx`, `telemetry.ts`, `sitter.ts`,
`effort.ts`, `Chat.tsx` (incl. `buildRenderItems`, `ToolRun` keying,
`handleRangeSelect` index provenance), `ChatInput.tsx`, `ChatToolbar.tsx`,
`Sidebar.tsx`, `StatusBar.tsx`, `ModelSelector.tsx` (incl. the effort-clamp
effect — the "one arrow click serialises both slots" clobber it could cause is
explicitly handled server-side at `handlers_models.go:87-109`),
`Message/{Message,index,Part,AssistantContent,UserContent,ToolActivityGroup,ActionRow,ThinkingPart,StandaloneThinking,IntermediateAssistantMessage,SummaryMessage,EditForm,CopyButton,CopyTurnButton,TimeBadge,DurationBadge,UsageBadge,AskQuestionBlock,parseAwaitingAnswer,textParts,blocks,useCollapseAllSignal}`,
`ForkSessionModal.tsx`, `ConfirmDialog.tsx`, `Login.tsx`, `TodoList.tsx`.

---

## §4 — Build / check state at HEAD

- `pnpm typecheck` (`tsc --noEmit`) — clean.
- No Go package was modified by this review; no Go tests run (per the
  no-project-wide-suites instruction, and nothing in scope to scope them to).
- Working tree unchanged by this review. Both harnesses live in the OS temp dir,
  not in the repo.

---

## §5 — Recommendation to the orchestrator

Both findings are in one file and can go in **one commit**.

1. **M-1 first** — it is the behavioural one. The fix must decide what `done`
   means; the honest answer is "the sub-agent's session is no longer busy",
   which `isRunning` already computes at `:75`. Whatever is chosen, delete the
   local `isFinished` rather than patching it, so there is one lifecycle
   predicate in `web/` and not two. Sweeping the three sites in the §2 table at
   the same time is cheap and removes the whole class.
2. **M-2 is a comment.** Same commit, no decision needed.
3. There is **nothing to file in the backend** this round, and nothing in
   `web/src/ws.ts`, `web/src/useWS.ts` or `web/src/store.ts`. The three commits
   under review are correct.

On the exit condition: this round is not it, but the gap is closing and the
remaining density is now measurably in one component rather than "somewhere in
`web/`". A twenty-first round that fixes M-1 and M-2 and then re-reads
`SubAgentBlock.tsx` plus the four modals with the unsubscribed handlers has a
real chance of coming back empty.
