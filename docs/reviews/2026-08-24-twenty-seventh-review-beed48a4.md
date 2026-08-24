# Round 27 review — `crush` fork @ `beed48a4`

**Verdict: NO-GO.** One P3: `Sidebar.tsx`'s session-delete flow (`confirmDelete()` /
`confirmDeleteOtherSessions()`) removes a session row from the sidebar
optimistically, before the server confirms the delete — a realistic
server-side rejection (DB busy/locked, matching this fork's own justification
for round 26's F-1) leaves the sidebar showing a session as gone while it
still exists server-side, until the next periodic `list_sessions` poll
(≤5s later) silently restores it. Real and reproducible, but bounded and
self-healing — a materially smaller blast radius than round 26's F-1, hence
P3 rather than P2. `delete_other_sessions` additionally replies a blanket
`EventResponse "ok"` even when a per-session delete failed server-side
(handler only `slog.Warn`s), so a partial failure is invisible everywhere,
not just in the sidebar.

Round 26's fix (`beed48a4`, F-1: `SystemPromptModal.save()` now waits for the
server's reply) was re-derived from `git show beed48a4` and checked against
the finding it claims to fix — it holds exactly as described. See "Swept,
nothing filed" §1 for detail.

---

## F-1 (P3) — Sidebar session delete is optimistic: a rejected `delete_session`/`delete_other_sessions` still wipes the row locally, self-healing only after the next ≤5s poll

### Where

`web/src/components/Sidebar.tsx:48-63`:

```tsx
function confirmDelete() {
  if (!pendingDelete) return;
  ws.send("delete_session", { sessionID: pendingDelete.id });
  removeSession(pendingDelete.id);
  if (activeID === pendingDelete.id) setActiveSession(null);
  setPendingDelete(null);
}

function confirmDeleteOtherSessions() {
  if (!activeID) return;
  ws.send("delete_other_sessions", { keepID: activeID });
  for (const s of allSessions) {
    if (s.ID !== activeID) removeSession(s.ID);
  }
  setConfirmDeleteOthers(false);
}
```

Both call `removeSession(id)` (`store.ts:115-117`, an unconditional
`$sessions.set(filter(...))`) in the same tick as `ws.send`, with no `msgID`,
no reply listener, no wait for anything to come back over the wire — the
same fire-and-forget shape round 26's F-1 fixed in `SystemPromptModal`, just
one layer removed: here the optimistic mutation is a row *removal* from a
list-backed store rather than a local `dirty`-flag reset.

### The failure path is real, not hypothetical

`internal/server/handlers_sessions.go:93-104`:

```go
func handleDeleteSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Sessions.Delete(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
```

`a.Sessions.Delete` (`internal/session/session_lifecycle.go:72-101`) is a
3-statement transaction (`DeleteSessionMessages` → `DeleteSessionFiles` →
`DeleteSession`, then `tx.Commit()`) preceded by a `GetSessionByID` inside
the same tx — any step can fail under the exact conditions round 26's own
F-1 established as realistic for this fork (DB busy/locked from N concurrent
`crush run` sessions against the same data directory; the row already gone
from a race with another tab). On any of those, `Delete` returns a non-nil
`err`, the handler replies `EventError`, and — critically — **no
`session_deleted` broadcast fires**, because `s.Publish(pubsub.DeletedEvent, ...)`
(line 99) is only reached after a successful `tx.Commit()`.

`delete_other_sessions` (`handlers_sessions.go:113-139`) is worse: a
per-session failure inside its loop is swallowed entirely —

```go
if err := a.Sessions.Delete(ctx, s.ID); err != nil {
    slog.Warn("delete_other_sessions: failed to delete session", "id", s.ID, "err", err)
}
```

— logged server-side only, then the handler still replies
`EventResponse{"status": "ok"}` unconditionally (line 138) regardless of how
many of the loop's deletes actually failed. There is no way for the client
to learn a specific session survived; the "ok" reply is not honest about
partial failure.

### Concrete failure scenario

Operator has two sessions open in two tabs (or one tab, doesn't matter for
this scenario), deletes "Session Two" from the sidebar. The row vanishes
immediately. If the delete then fails server-side (DB contention from a
concurrent session, or a race where something else already removed the
row), the operator sees:

- The sidebar row is gone — reads as "deleted successfully."
- The only failure signal is the same global 8-second transcript banner
  round 26's F-1 flagged as an inadequate error surface (`useWS.ts:312-315`,
  `Chat.tsx:459-464`) — not anywhere in or near the sidebar itself.
- If the operator reloads the page, switches away and back, or just waits,
  within 5 seconds the row **reappears** on its own (see Reproduction) —
  the true server state resurfaces via the periodic `list_sessions` poll
  (`useWS.ts:349-356`, `sessions_list` handler at `useWS.ts:124-126`
  unconditionally calls `setSessions(sessions)`, replacing `$sessions`
  wholesale with the authoritative list).
- For the ≤5s window, the sidebar is simply wrong: a session the operator
  believes gone is still fully intact and billable/consuming DB space, with
  no operator action possible on it (it's not clickable — it isn't there).

For `delete_other_sessions`: operator clicks "Delete all other sessions",
confirms. Every non-active row vanishes at once. If one of N sessions fails
to delete server-side, the client still receives an unqualified "ok" reply —
there is no way, even in principle, for this UI to ever learn which session
survived; the row for that session simply reappears silently on the next
poll with no indication anything was ever wrong.

### Why this is P3, not P2 (unlike round 26's F-1)

Round 26's F-1 was P2 because the stale state was **not self-correcting**:
`SystemPromptModal` has no periodic re-fetch, so a rejected save stayed
silently lost until the operator manually reopened the modal (and even then,
nothing on screen explained why). Here, `Sidebar`'s `$sessions` IS
periodically reconciled against server truth every 5 seconds regardless of
any user action — confirmed below. The defect is real and user-visible (a
false "it's deleted" belief for up to 5 seconds, and a dishonest "ok" reply
masking partial failure in the multi-delete path) but it cannot cause
permanent data loss, misdirected messages, or a corrupted number of
sessions — every variant tested self-heals without the operator doing
anything.

### Reproduction

Live Playwright specs against the real dev server (`pnpm dev` on
`:3131`), using the repo's own `web/tests/helpers/mock-ws.ts` and
`fixtures.ts` verbatim (copied, not modified), all outside the repo.

**`delete-session-failure.spec.ts`** — three tests:

```
  ok  CONTROL: successful delete_session removes the row and stays removed
  ok  BUG: a rejected delete_session still removes the row client-side, with
      only a transient (self-healing) fix via the next 5s poll
  ok  BUG: delete_other_sessions replies EventResponse "ok" even when a
      per-session delete failed server-side, and the client has already
      wiped it from the sidebar

  3 passed
```

The CONTROL confirms a genuine `session_deleted` broadcast keeps the row
gone (the correct shape) — not a harness artifact. Both BUG tests inject a
real `{ type: "error", id: <msgID>, error: "database is locked" }` frame (or,
for the second, a blanket `{ type: "response", payload: {status: "ok"} }`)
matching `handlers_sessions.go`'s exact wire shape, confirm the row is gone
with the server never actually having deleted it, then confirm the row
reappears once a `sessions_list` frame carrying the true (unchanged) list
arrives.

**`delete-last-session.spec.ts`** — a sharper edge case checked and ruled
out as a *further* hazard: deleting an operator's **only** session and
having that delete rejected. Confirmed the empty-sidebar state during the
failure window does **not** cascade into a spurious `create_session` (which
would have produced a duplicate session once the real one reappeared) —
`useWS.ts`'s `sessions_list` handler gates auto-create on
`topLevelSessions.length === 0` computed from the **server-reported** list
(`useWS.ts:141-144`), not from local optimistic state, so this specific
compounding risk does not exist:

```
  ok  single session + rejected delete: sidebar shows empty state but does
      NOT spuriously create_session, because auto-create is gated on the
      server's list, not local optimistic state

  1 passed
```

### Fix direction (not applied — read-only review)

Same pattern as round 26's F-1: give `confirmDelete()`/
`confirmDeleteOtherSessions()` a `msgID` and a one-shot reply listener; only
call `removeSession`/multi-`removeSession` after an `EventResponse`, and on
`EventError` leave the row(s) in place with an inline error (e.g. next to
the delete button, or re-using `ConfirmDialog`'s space before it closes)
rather than relying solely on the transcript-pane banner. For
`delete_other_sessions` specifically, the backend should also stop replying
an unqualified "ok" when the per-session loop recorded failures — e.g.
return the list of session IDs that actually got deleted (or that failed)
so the client only removes rows the server actually removed, rather than
either removing everything unconditionally or trusting a lossy "ok".

---

## Swept, nothing filed

So round 28 does not re-walk this ground.

1. **`beed48a4` (round 26 F-1) re-derived from its actual diff and verified
   against the finding it claims to fix.** `ChatToolbar.tsx`'s
   `SystemPromptModal.save()` now generates a `msgID`, sends it via `ws.send`'s
   `id` parameter, and registers a one-shot `ws.on("*", ...)` reply listener
   matching `MCPForm.submit()`'s shape exactly, including an `unsubRef`
   detached on unmount. `dirty`/`saving` stay unresolved until the reply
   arrives; `EventError` leaves `dirty` true and renders the error inline in
   the modal footer without clearing the draft. Matches the commit message
   and round 26's F-1 description with no gaps.
2. **Full enumeration of every `ws.send(...)` call site in `web/src`**
   (`grep -rn 'ws\.send(' web/src`, ~65 sites across 20 files) to look for a
   FOURTH mutating form still missing the msgID-scoped reply-listener
   pattern, per this round's suggested starting point:
   - `ScopedModelsModal.tsx`'s `setScoped`/`clearScoped` (`:327-332`) send
     with no `msgID` — checked and confirmed correct-by-design: unlike
     `SystemPromptModal`, this modal holds **no local optimistic state at
     all**; its only state (`scopedModels`) is written exclusively by the
     `scoped_models` broadcast listener (`:310-312`), and
     `handleSetScopedModel`/`handleClearScopedModel`
     (`internal/server/handlers_models.go:304-388`) broadcast a fresh
     `EventScopedModels` snapshot to all clients on every successful
     write. On error, no broadcast fires and no local state changed either
     — nothing to reconcile. This is the *correct* alternative pattern
     (server-state-driven rather than optimistic), not a bug.
   - `MCPSettings.tsx`'s `ServerRow.toggle()` (`:203-205`, `set_mcp_disabled`)
     sends with no `msgID` and touches no local state — `info.disabled`
     comes from `$mcpState`, populated exclusively by the `mcp_state`
     broadcast (`useWS.ts:277-278`), itself fed by
     `mcp.SubscribeEvents`/`broker.Publish` inside
     `DisableServer`/`EnableServer`/`updateState`
     (`internal/agent/tools/mcp/init.go:299-354,454-475`) via the
     `events.go:96-109` pubsub bridge. Same correct broadcast-driven
     pattern as `ScopedModelsModal` — confirmed the publish call exists on
     both the disable and enable paths, not just one. On error, the toggle
     icon simply doesn't move (true-negative, not a false-positive "looks
     saved" the way F-1 was) — not filed.
   - `LogsModal.tsx`'s `fetchLogs()` (`:21-42`, `get_logs`, read-only) —
     already implements the msgID-scoped reply-listener pattern correctly,
     with `unsubRef` cleanup on unmount identical to `MCPForm.submit()`.
     Confirms this pattern is well-established outside the three
     write-forms explicitly named in the prompt.
   - `ForkSessionModal.tsx`'s `confirm()` (`:21-25`, `fork_session`) —
     fire-and-forget by design: the modal closes immediately
     (`onClose()`) regardless of outcome, and the actual navigation to the
     new session happens later, driven entirely by the
     `session_created` broadcast (`useWS.ts:101-112`), which `setActiveSession`s
     and `load_messages`s only once the server confirms. A failed fork
     leaves the operator exactly where they started (no new session,
     modal closed, nothing removed or falsely marked complete) — the
     closest local analogue to `ScopedModelsModal`'s "no optimistic state
     to go stale" shape. Not filed.
   - `Sidebar.tsx`'s `selectSession`/`newSession`/`saveRename` — all three
     checked: `selectSession` (`:33-37`) and `newSession` (`:39-41`) mutate
     no state that isn't immediately corrected by a subsequent broadcast/poll
     (`setActiveSession` before `load_messages` returns is a navigation
     intent, not a persisted-state claim). `saveRename` (`:71-76`) does
     **not** optimistically rename the row — it only closes the edit UI
     (`setEditingID(null)`) and waits for `session_updated`
     (`useWS.ts:113-116`) to actually apply the new title via `upsertSession`;
     on a rejected rename the title silently stays whatever the server's
     last-known value was — correct, not misleading. Only
     `confirmDelete`/`confirmDeleteOtherSessions` (F-1 above) actually
     mutate local list state ahead of confirmation.
   - `store.ts`'s remaining un-awaited sends (`update_todos`,
     `set_session_models` ×3, `set_theme`, `set_keep_alive`,
     `set_provider_key`/`remove_provider_key`, `delete_message`/
     `delete_messages`, `update_message_content`/`update_message_thinking`,
     `summarize_session`/`cancel_queued_summarize`,
     `delete_message_part`/`update_message_part`, `toggle_pin_message`,
     `rerun_message`, `add_context_path`/`remove_context_path`,
     `add_skills_path`/`remove_skills_path`) — spot-checked each for a
     matching local-optimistic-mutation-before-reply shape; all either (a)
     already carry a `msgID` + reply listener at their call site
     (`initialize_project`, `add_custom_provider`, `remove_custom_provider`,
     `update_custom_provider`, `set_provider_peak_hours` — all in
     `ProvidersModal.tsx`'s `submit()`/`sendAndWait()`, task #… lineage
     from round 21/25), or (b) are broadcast-reconciled the same way as
     `ScopedModelsModal` (messages/todos/pins all flow back through
     `message_updated`/`session_updated`-family broadcasts that `store.ts`'s
     own listeners in `useWS.ts` apply authoritatively), or (c) are genuine
     fire-and-forget telemetry/tracking calls
     (`track_model_usage`, `remove_recent_model`, `log_client_error`,
     `log_client_event`) with no user-visible state to go stale. No new
     candidate found beyond F-1 above.
3. **Go backend** — not reopened per the round-19 closure. The only backend
   code read this round (`handlers_sessions.go`, `session_lifecycle.go`,
   `handlers_models.go`, `handlers_mcp.go`, `internal/agent/tools/mcp/init.go`,
   `events.go`) was read exclusively to trace the *client-observable*
   consequences of F-1 above and to rule out the `ScopedModelsModal`/
   `MCPSettings` candidates — no new backend-only defect found, and F-1
   itself is a web/ bug (the backend's `EventError` reply is correct; the
   client simply doesn't wait for it).

## Reproduction artefacts

All outside the repository, under `D:\system_artefact\Temp\crush-r27\`
(harness config/helpers copied verbatim from round 26's
`D:\system_artefact\Temp\crush-r26\`, which remains on disk from the prior
round). The working tree was not modified — `git status --porcelain` shows
only the pre-existing `D web/dist/.gitkeep` and untracked `dev/`/`docs/`
entries that predate this session.

| path | what |
|---|---|
| `pw.config.ts` | Playwright config, `testDir ./spec`, `baseURL http://localhost:3131` (dev server), copied from round 26 |
| `helpers/mock-ws.ts`, `helpers/fixtures.ts` | verbatim copies of the repo's own `web/tests/helpers/*` (via round 26's copy) |
| `spec/delete-session-failure.spec.ts` | F-1: control (successful delete) + two bug tests (single rejected delete, `delete_other_sessions` masking a partial failure) |
| `spec/delete-last-session.spec.ts` | Follow-up check ruling out a duplicate-session-creation cascade when the operator's only session hits this failure path |

To re-run: `cd /d/system_artefact/Temp/crush-r27 && cmd //c "mklink /J node_modules D:\dev\go\crush\web\node_modules"`,
then `node_modules/.bin/playwright.CMD test --config=pw.config.ts` (dev
server, auto-started via the config's `webServer` block).
