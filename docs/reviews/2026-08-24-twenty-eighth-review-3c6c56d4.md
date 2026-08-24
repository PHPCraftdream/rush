# Round 28 review — `crush` fork @ `3c6c56d4`

**Verdict: NO-GO.** One P3: `useWS.ts`'s `messages_list` handler applies any
incoming reply unconditionally by session-ID match, with no `msgID` or
sequence/version check — for an externally-owned session (another `crush
run` process holds the lock), two independently-timed polling loops each
fire their own uncoordinated `load_messages` request for the same session,
and the server processes both through a 12-worker pool with no FIFO
guarantee across workers. If the reply to the *older* request is delivered
*after* the reply to a *newer* one, the client's rendered transcript
silently regresses — a message the operator was already looking at
disappears — until the next poll tick (≤1.5s later) corrects it. Same
severity class and blast radius as round 27's F-1: real, user-visible,
self-healing, no data loss.

Round 27's fix (`3c6c56d4`, F-1: Sidebar session-delete now waits for the
server) was re-derived from `git show 3c6c56d4` and checked against the
finding it claims to fix — it holds exactly as described. Both the client
diff (`msgID` + one-shot reply listener in `confirmDelete()`/
`confirmDeleteOtherSessions()`, `ConfirmDialog` gaining `error`/`busy`
props) and the server diff (`DeleteOtherSessionsResult{DeletedIDs,
FailedIDs}` replacing the bare `{"status":"ok"}`) match the commit message
precisely, and the new Go tests
(`internal/server/p684_delete_other_sessions_test.go`) genuinely assert
against real DB state post-delete, not just the reply shape. See "Swept,
nothing filed" §1.

---

## F-1 (P3) — `messages_list` replies are applied with no ordering guard: two concurrent `load_messages` requests for the same externally-owned session can have their replies arrive out of order, regressing the visible transcript

### Where

`web/src/useWS.ts:209-237`:

```ts
ws.on("messages_list", (msg: WSMessage) => {
  const payload = msg.payload as
    | { SessionID?: string; Messages?: Message[] }
    | Message[]
    | undefined;
  let sid: string | undefined;
  let msgs: Message[] = [];
  if (Array.isArray(payload)) {
    msgs = payload;
    sid = msgs[0]?.SessionID;
  } else if (payload) {
    msgs = payload.Messages ?? [];
    sid = payload.SessionID ?? msgs[0]?.SessionID;
  }
  if (sid && isSubAgentSession(sid)) {
    setSubAgentMessages(sid, msgs);
    return;
  }
  // For the main chat: only apply if it's for the currently active
  // session (we might have polled in flight and the user switched).
  const activeID = $activeSessionID.get();
  if (sid && activeID && sid !== activeID) return;
  setMessages(msgs);
}),
```

`setMessages` (`web/src/store.ts:130-132`) is an unconditional full-array
replace of `$messages`:

```ts
export function setMessages(msgs: Message[]) {
  $messages.set(msgs);
}
```

The only guard is "is this reply for the currently-active session" — there
is no `msgID`, no sequence number, no timestamp comparison. **Any**
`messages_list` reply for the active session, however stale, is applied.

### Two independent, uncoordinated pollers hit the same target

`web/src/useWS.ts:128-138` (piggy-backed on the 5s `sessions_list` poll):

```ts
// Foreign-owned active session: kick a load_messages refresh on
// every sessions_list poll too. This guarantees we never sit
// longer than the sessions poll interval (5s) without a fresh
// history read, in case the dedicated 1.5s messages poll
// missed a window during a pause.
const activeID0 = $activeSessionID.get();
if (activeID0) {
  const a = sessions.find((s) => s.ID === activeID0);
  if (a && a.OwnedExternal) {
    ws.send("load_messages", { sessionID: activeID0 });
  }
}
```

`web/src/useWS.ts:341-347,355-356` (dedicated 1.5s follow poll):

```ts
const pollMessagesIfFollowed = () => {
  const id = $activeSessionID.get();
  if (!id) return;
  const sess = $sessions.get().find((s) => s.ID === id);
  if (!sess || !sess.OwnedExternal) return;
  ws.send("load_messages", { sessionID: id });
};
...
messagesInterval = window.setInterval(pollMessagesIfFollowed, FOLLOW_MESSAGES_POLL_MS);
```

Both timers run concurrently and independently whenever the active session
is `OwnedExternal` (a session currently held by a *different* `crush run`
process — the fork's core multi-session design, see
`internal/server/handlers_sessions.go:170-214`'s
`annotateExternalOwnership`/`AnnotateSessionExternalOwnership`, gated on a
live lock file held by a different PID). At the 5s mark the two timers can
land within low hundreds of milliseconds of each other (e.g. t=4500ms from
the 1.5s timer and t=5000ms from the 5s timer), each independently sending
`load_messages` for the same session ID with no deduplication, no
in-flight tracking, and no `msgID` correlation on the client side at all —
`ws.send("load_messages", { sessionID })` never passes an `id`.

### Why replies can actually arrive out of send order

`internal/server/handlers.go:58-59` dispatches `load_messages` through the
generic work queue:

```go
case CmdLoadMessages:
    c.dispatch("handleLoadMessages", msg.ID, func() { handleLoadMessages(ctx, a, c, msg) })
```

`internal/server/hub.go:147-157` (`startWorkers`) drains that queue with
`workerPoolSize` (= `maxConcurrentHandlersPerConn` = 12) **independent**
goroutines pulling from one shared channel:

```go
func (c *Client) startWorkers() {
    c.workersOnce.Do(func() {
        for i := 0; i < workerPoolSize; i++ {
            go func() {
                for item := range c.workQueue {
                    runRecovered(item.name, func() {}, item.fn)
                }
            }()
        }
    })
}
```

Two `load_messages` work items queued moments apart can be picked up by
two *different* workers and execute concurrently; whichever worker's
`a.Messages.List(ctx, p.SessionID)` DB read (`handlers_messages.go:89-112`)
completes first calls `c.reply()` first, which is what actually determines
wire order (via `c.send`, drained FIFO by `writePump`). There is no
guarantee the request sent first is the request whose DB read — and
therefore whose reply — completes first. A slower read (e.g. a session
with more accumulated history, or ordinary DB/goroutine-scheduling jitter)
can easily let a request sent *earlier* finish, and therefore arrive at the
client, *after* a request sent *later*.

### Concrete failure scenario

Operator has the web UI open, watching a session that's actively being
driven by a *separate* `crush run` process (a first-class, documented
fork feature — CLAUDE.md: "fork's multi-session engine... built for N
truly concurrent `crush run` sessions"). Because the writer is a different
OS process, this server's own in-process pubsub (`message_created`/
`message_updated` broadcasts via `events.go`) never fires for this
session — that's the documented reason the 1.5s poll exists at all
(`useWS.ts:326-327`: "poll its `messages_list` every 1.5s so the
conversation streams visibly without going through that other process's
in-memory pubsub"). So unlike most other client state in this codebase,
there is no broadcast fallback to correct a bad `messages_list` apply —
only the *next* poll tick fixes it.

Sequence:
1. External process appends message 3 to the session.
2. At t=4500ms the 1.5s timer fires `load_messages` (request **OLD**).
3. At t=5000ms the 5s `sessions_list`-triggered refresh independently
   fires another `load_messages` for the same session (request **NEW**).
4. Request **NEW**'s DB read happens to complete first (e.g. request
   **OLD**'s worker goroutine was scheduled slightly later, or its query
   raced a slower disk access) — its reply lands on the client first,
   showing all 3 messages, including the just-written message 3. The chat
   view is correct and current.
5. Request **OLD**'s reply — a snapshot taken *before* message 3 existed —
   arrives second. `useWS.ts`'s handler has no way to know this reply is
   older than the one it already applied; it unconditionally calls
   `setMessages(msgs)` with only 2 messages.
6. The operator's screen loses message 3. It reappears once the *next*
   poll tick lands (at most ~1.5s later under normal timer cadence, though
   a second unlucky reordering could extend that further).

This is bounded and self-healing (same reasoning as round 27's F-1— no
permanent data loss, the DB is untouched, only the client's rendered view
regresses transiently), hence **P3**, not P2.

### Reproduction

Attempted a live Playwright run first, per the review's standard method,
using a config/helpers set copied verbatim from round 27's harness
(`D:\system_artefact\Temp\crush-r27\pw.config.ts`,
`helpers/mock-ws.ts`, `helpers/fixtures.ts`) into
`D:\system_artefact\Temp\crush-r28\`. This machine's `web/node_modules`
turned out to have **pre-existing, unrelated breakage** that blocks
booting the dev server at all: `@alloc/quick-lru` is missing from the pnpm
store entirely, and `@babel+parser@7.29.0`'s extracted package directory
is empty (its `package.json` ENOENT's) — confirmed via `ls -la` that both
predate this session by weeks (`@babel+parser`'s directory mtime is
2026-07-26). `pnpm install --frozen-lockfile` surfaced the same class of
corruption in more packages before I stopped rather than risk a disruptive
reinstall in a workspace other agents may be actively using concurrently.
This is an environment/tooling issue, unrelated to any code under review,
noted here so round 29 doesn't waste time re-diagnosing it as new.

Fell back to the prompt's explicitly authorized alternative: a standalone
Node script (`D:\system_artefact\Temp\crush-r28\messages-list-reorder-node-harness.mjs`)
that imports the **real handler logic verbatim** — the exact body of
`useWS.ts:209-237`'s `messages_list` handler (only the wrapping
`ws.on("messages_list", ...)` registration removed, no logic changed),
plus `store.ts:130-132`'s `setMessages` and `store.ts:277-279`'s
`isSubAgentSession`, copied character-for-character — against a minimal
nanostores-behavior-equivalent `atom()` stand-in (get/set/subscribe only;
not a reimplementation of any of the logic under test). Output:

```
ok   after the NEWER reply: chat shows ["First message","Second message","Third message (latest)"]
     after the OLDER (stale) reply arrives second: ["First message","Second message"]
FAIL: BUG REPRODUCED: the real messages_list handler (useWS.ts:209-237, copied verbatim)
      applied the stale, older reply unconditionally, silently dropping "Third message (latest)"
      from the chat the operator was already looking at. No msgID/sequence check exists to reject
      an out-of-order reply for the same, still-active session.
ok   CONTROL (replies in send order): final state correct: ["First message","Second message","Third message (latest)"]
```

(The harness intentionally exits with the "FAIL" line for the bug
scenario — that line documents the predicted defect firing, not a harness
error; the preceding and following `ok` lines are the control
assertions that bracket it, confirming the harness itself behaves
correctly both before and after.)

The control at the bottom proves this isn't an artifact of the harness:
delivering the same two replies in send order (old, then new) produces the
correct, non-regressing final state — the defect only manifests under
genuine reordering, exactly as the server-side worker-pool analysis
predicts is possible.

Server-side reasoning (no live-server test needed to establish this part —
it follows directly from reading `hub.go`/`handlers.go`, cited above) is
that two `load_messages` work items for the same connection are **not**
guaranteed to complete, and therefore reply, in send order, because they
can be picked up by two of the 12 independent pool workers and race each
other's DB reads.

### Fix direction (not applied — read-only review)

Give `load_messages` a client-generated `msgID` per send (matching the
pattern already used for every write-form in this codebase per rounds
26-27's fixes) and track only the most-recently-sent request's ID per
session; on receiving a `messages_list` reply, ignore it if a newer
request for the same session has since been sent and not yet answered
(i.e. drop replies to superseded requests, keep only the latest). This is
the standard "stale-response guard" pattern and does not require any
server-side change — the server already echoes `id` back in `c.reply`'s
envelope (`hub.go:544-557`). Alternatively, the two pollers could be
merged into one so only ever one `load_messages` is in flight per session
at a time, removing the race at its source rather than papering over it
client-side.

---

## Swept, nothing filed

So round 29 does not re-walk this ground.

1. **`3c6c56d4` (round 27 F-1) re-derived from its actual diff and
   verified against the finding it claims to fix.** Client:
   `confirmDelete()`/`confirmDeleteOtherSessions()`
   (`web/src/components/Sidebar.tsx`) now generate a `msgID`, register a
   one-shot `ws.on("*", ...)` reply listener with `unsubRef` cleanup on
   unmount, and only mutate `$sessions` on a genuine `EventResponse`;
   `EventError` leaves the row(s) in place with the error rendered inline
   via `ConfirmDialog`'s new `error`/`busy` props. Server:
   `handleDeleteOtherSessions`'s reply changed from a bare
   `{"status":"ok"}` to `DeleteOtherSessionsResult{DeletedIDs, FailedIDs}`
   (`protocol.go`), populated from the existing per-session delete loop;
   the client only drops rows whose ID is echoed in `deletedIDs`. Two new
   Go tests (`p684_delete_other_sessions_test.go`) assert the reply
   payload against real DB state post-delete (not just the wire shape) for
   both a partial-failure and a full-success case. Matches the commit
   message and round 27's F-1 description with no gaps.
2. **Keyboard/focus-trap correctness across modals** (this round's first
   suggested angle) — checked every `document.addEventListener("keydown",
   ...)` site in `web/src` (`ChatToolbar.tsx` ×2, `ScopedModelsModal.tsx`,
   `SettingsModal.tsx`, `ConfirmDialog.tsx`, `ProvidersModal.tsx`,
   `LogsModal.tsx`, `MCPSettings.tsx`) for a stacking-modal hazard (two
   full modals mounted simultaneously, each with its own global Escape/
   Enter listener, racing on which one intercepts the keypress). Ruled
   out: every full-screen modal (`SystemPromptModal`, `MCPSettings`,
   `SettingsModal`, `ProvidersModal`, `ScopedModelsModal`, `LogsModal`) is
   opened exclusively from `ChatToolbar.tsx`'s single "More" menu
   (`:367-413`), one boolean flag per modal, and the menu closes
   (`setMoreMenuOpen(false)`) on the same click that opens any one of
   them — there is no UI path to have two of these six mounted at once.
   The only place a `ConfirmDialog` is layered over another already-open
   surface is `Sidebar.tsx` (delete confirmations) and several
   `Message/*` components (`ActionRow`, `ThinkingPart`,
   `IntermediateAssistantMessage`, `Chat.tsx`) — none of those parent
   components register their own `document`-level keydown listener
   (`Sidebar.tsx` uses only a per-input `onKeyDown` for rename, not a
   global listener), so `ConfirmDialog`'s own Escape/Enter handling never
   competes with anything. `MCPForm`'s Escape (`MCPSettings.tsx:141-143`)
   is scoped to its own textarea via React's `onKeyDown`, not
   `document`-global, so it can't leak into `MCPSettings`'s own Escape
   listener either. No stacking-modal keyboard hazard exists in this
   codebase today.
3. **`internal/server/hub.go`'s replay-ring/sticky-broadcast behavior**
   (this round's third suggested angle) — read in full against everything
   changed since round 24 last touched this area. The sticky-vs-replay
   ordering invariant (`Run`'s `register` case sends sticky envelopes
   *before* the replay buffer, specifically so a large replay can't fill
   `c.send` and starve a sticky send under the non-blocking `default:`
   drop) is still correctly implemented and its own doc comment
   (`hub.go:412-434`) proves the "never a superseded sticky copy last"
   guarantee holds given sticky envelopes are never stored in
   `replayBuffer` at all (confirmed: `BroadcastSticky` sends only on
   `stickyBroadcast`, never calls `buffer.push`). `replayBuffer`'s
   count/byte-budget/per-event-size triple bound (`hub.go:245-321`) is
   internally consistent — `push`'s eviction loop guards `count > 1` so
   the just-pushed event is never evicted by its own insertion, and
   `evictHead` correctly decrements `bytes` before advancing `head`. No
   defect found; this subsystem looks like a settled, heavily-hardened
   area from prior rounds, consistent with round 27's swept note that no
   new backend-only defect was found there either.
4. **Broadcast-vs-broadcast ordering** (this round's second suggested
   angle, the other half of it) — `message_created`/`message_updated`/
   `message_deleted` broadcasts for the SAME connection are not subject to
   the same worker-pool reordering risk as `load_messages`, because they
   are never dispatched as request-driven work items in the first place:
   they originate from `events.go`'s pubsub bridge calling `hub.Broadcast`
   directly from whichever goroutine the underlying event fired on, and
   `Hub.Run`'s single event loop (`hub.go:386-520`) processes the
   `broadcast` channel strictly one message at a time — there is no
   worker pool on the fan-out side, only on the per-connection *inbound*
   command-dispatch side. So a genuine same-type broadcast-vs-broadcast
   reordering (e.g. two `message_updated` events for the same message ID
   swapping order) is not possible for this hub; each `Broadcast` call is
   fully serialized through the single `Run` goroutine before the next one
   is read off the channel. The F-1 hazard above is specific to
   `load_messages`/`messages_list` precisely because that pair goes
   through the *request*-side worker pool (`dispatch`/`workQueue`), which
   `Broadcast` does not.
5. **`sessions_list`/`list_sessions`** — checked for the same class of
   hazard (two in-flight `list_sessions` requests racing). It is
   structurally exposed to the same non-FIFO worker-pool dispatch
   (`handlers.go`'s `CmdListSessions` also goes through `c.dispatch`), but
   ruled lower-risk and not filed as a second finding: `list_sessions` is
   sent from exactly one recurring 5s timer (`useWS.ts:355`) plus one
   immediate call on tab-focus (`startPolling`, `:353`) and one on
   `_connected` (`:80`) — the tab-focus and reconnect sends are one-shot,
   not recurring, so a genuine double-in-flight window is narrow (only
   right at the moment of a reconnect or focus event landing within the
   same tick as the 5s timer) compared to `messages_list`'s two
   *permanently coexisting* recurring timers for the entire time a session
   is `OwnedExternal`. Even when it does double up, `setSessions` replacing
   the list with a slightly-earlier-but-still-recent full snapshot is far
   less perceptually jarring than a chat transcript losing its newest
   message — sessions list rows don't have the same "actively being read,
   growing in real time" property. Noted as a theoretical sibling of F-1,
   not filed separately to avoid padding the report with a lower-confidence
   variant of the same root cause.
6. **Go backend** — not reopened per the round-19 closure beyond what was
   necessary to trace F-1's server-side origin (`handlers.go`,
   `hub.go`, `handlers_messages.go`, `handlers_sessions.go`). No new
   backend-only defect found; F-1 is a genuine client-side gap (missing
   stale-reply guard) with a real server-side contributing cause (the
   worker pool's lack of per-session FIFO ordering), consistent with how
   this loop has treated web/server co-located bugs in prior rounds.

## Reproduction artefacts

All outside the repository, under `D:\system_artefact\Temp\crush-r28\`.
The working tree was not modified — `git status --porcelain` shows only
the pre-existing `D web/dist/.gitkeep` and untracked `dev/`/`docs/`
entries that predate this session.

| path | what |
|---|---|
| `pw.config.ts`, `helpers/mock-ws.ts`, `helpers/fixtures.ts` | copied verbatim from round 27's `D:\system_artefact\Temp\crush-r27\`, itself copied from the repo's own `web/tests/helpers/*` |
| `spec/messages-list-reorder.spec.ts` | Playwright spec for F-1 (control + bug case), written but **not run to completion** — this machine's `web/node_modules` has pre-existing, unrelated corruption (`@alloc/quick-lru` missing entirely; `@babel+parser@7.29.0`'s extracted directory empty, both dated 2026-07-26, weeks before this session) that prevents `rsbuild dev` from booting at all, via both `pnpm dev` and a direct `node .../@rsbuild/core/bin/rsbuild.js dev` invocation. `pnpm install --frozen-lockfile` surfaced further corruption in the same store before I stopped short of a full reinstall, to avoid disrupting other agents potentially using the same shared `web/node_modules`. Left in place for round 29 or a maintainer to either use directly (once the environment is fixed) or disregard. |
| `messages-list-reorder-node-harness.mjs` | **The actual reproduction evidence for F-1.** Standalone Node (v24) ESM script with zero dependency on the broken `web/node_modules` — imports nothing from it; instead copies the real `useWS.ts:209-237` handler body and `store.ts:130-132`/`store.ts:277-279` verbatim, against a minimal nanostores-equivalent `atom()` stand-in. Confirmed reproduces the regression deterministically, with a control proving in-order delivery does not trigger it. Rerun: `node D:\system_artefact\Temp\crush-r28\messages-list-reorder-node-harness.mjs` |

To fix the environment for a future round's live Playwright run: the
`web/node_modules` pnpm store on this machine needs a genuine repair (a
full `pnpm install` outside of any concurrent-agent window, or a check of
whatever process last modified `node_modules/.pnpm/@babel+parser@7.29.0`
around 2026-07-26 for why the extraction was incomplete) before
`rsbuild dev` will boot again.
