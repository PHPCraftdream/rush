# Round 26 review — `crush` fork @ `4aeb475d`

**Verdict: NO-GO.** One P2: the System Prompt modal's Save button marks the edit
"saved" (Save disabled, Reset hidden) synchronously on click, before any server
round-trip — so a realistic server-side failure (busy/locked DB, or the session
being deleted concurrently by another tab/process, both live possibilities under
this fork's multi-session design) is invisible in the modal; the operator's edit
is silently discarded and reopening the modal shows the old content with no
explanation once the (differently-located, 8-second) error banner has vanished.

All four of round 25's fixes (`ff7adbcd` F-3, `76d99653` F-2, `0a877228` F-1,
`4aeb475d` F-4) were re-derived from their diffs and cross-checked against the
finding they claim to fix — all four hold exactly as described, with no
regressions introduced by the refactors (confirmed every call site of the
changed `dequeueAllMessages`/`sendWithFastModel`/`enqueueMessage` signatures is
consistent). See *Swept, nothing filed* §1 for the detail.

---

## F-1 (P2) — `SystemPromptModal.save()` is fire-and-forget: it marks the edit clean before the server has even seen it, so a rejected save is silently lost

### Where

`web/src/components/ChatToolbar.tsx:59-64`:

```tsx
function save() {
  setSaving(true);
  ws.send("set_system_prompt", { sessionID, content: draft });
  setOriginal(draft);
  setSaving(false);
}
```

`dirty` (`:36`) is `draft !== original`, and the UI keys everything off it:

```tsx
const dirty = draft !== original;
...
{dirty && (
  <button onClick={reset} ...>Reset</button>
)}
...
<button onClick={save} disabled={!dirty || saving} ...>
```

`save()` sets `original = draft` **synchronously, in the same tick as the
click**, with no `msgID`, no reply listener, and no wait for anything to come
back over the wire. `saving` is set `true` then immediately `false` in the same
function body, so the spinner state (if any is rendered off it) never has a
chance to show either. The moment Save is clicked, `dirty` becomes `false`:
the Save button disables and the Reset button disappears — completely
independent of whether the write actually happened.

Compare every other mutating form audited this round: `MCPForm.submit()`
(`MCPSettings.tsx`) registers a `ws.on("*", ...)` reply handler scoped to a
`crypto.randomUUID()` `msgID` and only resolves its local "busy" state once
that reply arrives; `ProvidersModal`'s `submit()`/`sendAndWait()` (rounds 21 and
25's `76d99653`) do the same. `SystemPromptModal` is the outlier: it has no
reply-awaiting mechanism of any kind.

### The failure path is real, not hypothetical

The backend handler can and does reply with `EventError`:

`internal/server/handlers_sessions.go:272-287`:

```go
func handleSetSystemPrompt(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetSystemPromptPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}
	if err := a.AgentCoordinator.UpdateSessionSystemPrompt(ctx, p.SessionID, p.Content); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
```

`UpdateSessionSystemPrompt` → `session.service.UpdateSystemPrompt`
(`internal/session/session_update.go:98-109`) is a plain
`UPDATE sessions SET system_prompt=? WHERE id=?` with no existence
pre-check — any SQLite write error (busy/locked under concurrent write load,
which this fork's whole design explicitly anticipates: N concurrent `crush run`
sessions against the same data directory) or the session having been deleted a
moment earlier by another tab/process (`DeleteSession` is a first-class,
reachable action in this multi-client model) surfaces as a genuine `err` here,
which the handler correctly turns into an `EventError` reply.

But that reply lands nowhere useful. `EventError` frames are consumed by a
single **global**, non-request-scoped listener:

`web/src/useWS.ts:312-315`:

```ts
ws.on("error", (msg: WSMessage) => {
  $agentError.set((msg.error as string) || "Unknown error");
  setTimeout(() => $agentError.set(null), 8000);
}),
```

`$agentError` is rendered exactly once, in `Chat.tsx:459-464` — inside the
**transcript pane**, not the modal — and auto-dismisses after 8 seconds
regardless of whether anyone saw it. `grep`ing the rest of `web/src` confirms
`SystemPromptModal` has no other subscription to `system_prompt`-related state:
it is entirely local `useState`, with no store involvement to catch the error
some other way.

### Concrete failure scenario

Operator opens **More → System Prompt**, edits the text, clicks **Save**. The
button greys out and Reset vanishes instantly — reads as "saved". If the write
then fails (DB contention from a concurrent session, or the session having just
been deleted in another tab), the *only* signal is a red banner that appears in
the chat transcript behind/beside the still-open modal, for 8 seconds. If the
operator's attention is on the modal they just interacted with (the common
case — they're still looking at what they just clicked), they miss it entirely.
Closing and reopening the modal re-fetches via `get_system_prompt` and shows
the true, never-actually-saved original content, with nothing left on screen at
that point to explain why their edit is gone.

### Reproduction

Live Playwright spec against the real dev server AND the production bundle
(`web/dist` served read-only), using the repo's own `tests/helpers/mock-ws.ts`
and `tests/helpers/fixtures.ts` verbatim (copied, not modified) — all outside
the repo. A control run first confirms the *correct* shape (a successful
round-trip does the right thing), then the bug tests inject a real
`{ type: "error", error: "database is locked" }` frame after `set_system_prompt`
is sent, matching `handlers_sessions.go`'s exact wire shape:

```
  ok  CONTROL: happy path — Save disabled after success, as expected

  x   modal marked clean before server replied: true
  x   modal STILL shows clean after server sent EventError("database is locked"):
      Save disabled=true, Reset hidden=true

  x   reopening the modal shows the OLD content — the operator's edit is gone
      with no error visible in the modal at any point in this sequence

  3 passed (both dev server :3131 and production bundle :3132 — identical results)
```

All three tests **pass**, i.e. they successfully assert the buggy behavior
(named `BUG:` to make that unambiguous) — the control (`CONTROL:`) is what
demonstrates this isn't a harness quirk: the same modal, same mock transport,
same run, correctly reflects a *successful* save when the server actually
replies `EventResponse`. Ran against both `pnpm dev` and the prebuilt
production bundle with identical results, so this is not a StrictMode/dev-only
artifact (unlike round 25's F-2) — it's a straightforward missing-await bug
that manifests identically everywhere.

### Fix direction (not applied — read-only review)

Give `save()` a `msgID` and register a one-shot reply listener the way
`MCPForm.submit()`/`ProvidersModal` already do: keep `saving = true` and
`dirty` unresolved until the reply arrives; on `EventResponse` commit
`setOriginal(draft)`; on `EventError` leave `dirty` true, clear `saving`, and
show the error *inside the modal* (a small inline message reusing the existing
`msg.error` string) rather than relying solely on the global banner racing
against the modal's own z-index and an 8-second timeout.

---

## F-2 (Minor) — an attachment cannot be sent without accompanying text; the compose controls disable outright with no explanation, even though the backend fully supports content-less-but-attachment-bearing messages

### Where

`web/src/components/ChatInput.tsx:633`:

```tsx
const canSend = !!text.trim() && !!activeSessionID;
```

This gates every one of the four send affordances — Send/Queue (`:787-808`),
"Send with lightweight model" (`:753-762`), Interrupt (`:763-774`), and Inject
(`:775-786`) — via `disabled={!canSend}`. Attaching a file with an empty
textarea leaves `canSend` false: every button that could dispatch the
attachment stays disabled, silently, with no tooltip or message explaining why
(the disabled state looks identical to "no session selected").

This predates round 25's attachment-plumbing fix (`0a877228`) — `canSend`'s
text-only definition has been unchanged since before attachments existed at
all — so it is not a regression from that fix, just a pre-existing gap that
round 25's fix didn't happen to touch (F-1 there was about attachments being
silently *dropped when combined with text*, not about attachment-only sends
being blocked outright).

### Backend already supports this

`internal/server/handlers_agent.go:175-204`'s `handleSendMessage` builds
`p.Content += "\n[Attached file: " + savedPath + "]"` per attachment
unconditionally — it works correctly even when `p.Content` starts empty.
Neither `coordinator.Run` (`internal/agent/coordinator.go:382-396`) nor
`runInternal` (`internal/agent/coordinator_run.go:134+`) rejects an empty
`prompt`; there is no length/emptiness validation anywhere on that path.

### Concrete failure scenario

Operator wants to send a screenshot with no caption ("just look at this").
They attach the file — the badge appears — but every send button stays
disabled (`opacity-30`, per the shared `disabled:opacity-30` styling), and
nothing on screen explains that typing *any* text would unblock it. The
natural read is "attachments aren't supported here" or "something is broken."

### Severity

Minor, not P2/P3: no data is lost or silently mis-sent (contrast F-1 above and
round 25's own F-1, both of which involved an attachment vanishing after the
user believed it was sent) — the badge stays visible and truthful, the operator
just cannot submit until they type a token of text. Not filed as a numbered P-
finding; noted for completeness since it's adjacent to the area round 25 just
fixed and nobody has flagged it explicitly across 26 rounds.

---

## Swept, nothing filed

So round 27 does not re-walk this ground.

1. **All four of round 25's fixes re-derived from their actual diffs and
   verified to match their commit messages exactly, with no follow-on
   regressions:**
   - `ff7adbcd` (F-3): `Sidebar.tsx`'s `onBlur={saveRename}` is gone from the
     rename `<input>`; nothing else in the diff.
   - `76d99653` (F-2): both `ProviderForm` and `BuiltinProviderEditor` in
     `ProvidersModal.tsx` gained `disposedRef.current = false;` at the top of
     their mount effect, exactly as prescribed; `unsubRef`/`pendingUnsubs`
     untouched, matching the commit's own claim they needed no change.
   - `0a877228` (F-1): `ChatInput.tsx`'s `sendFast`/`interrupt`/`inject` now
     build `WireAttachment[]` via a shared `toWireAttachments` helper;
     `store.ts`'s `sendWithFastModel`/`enqueueMessage`/`dequeueAllMessages`
     signatures all changed consistently, and every call site (`ChatInput.tsx`,
     `useWS.ts`) was updated to match — no stale caller of the old
     `dequeueAllMessages(): string | undefined` signature remains anywhere in
     `web/src`.
   - `4aeb475d` (F-4): `ActionRow.tsx` gained a `collapse` callback that clears
     `editingThinking` alongside `setOverride(false)`, and `toggle` now routes
     through it — mirrors `ThinkingPart.tsx`'s existing pattern exactly.
2. **Dead-code lead, not filed: `internal/server/server.go:128`'s hardcoded
   `http://localhost:3000` dev-mode redirect.** The `static == nil` branch this
   sits in is genuinely unreachable in the shipped binary: `grep`ing every
   `server.New(...)` call site in the repo (including all `.claude/worktrees/*`
   copies) shows every single one passes `crushweb.FS()` (never nil) from
   `internal/cmd/root.go:174`. This is Go backend code with no live, reachable
   behavioral symptom — closed per round 19's standing rule, and would fail the
   "concrete behavioral bug, not a hypothesis" bar even if reopened, since
   nothing in the actual `crush` binary can ever hit this branch.
3. **Playwright port/origin uniqueness — purely tooling, confirmed, not a
   product-code hazard.** `web/playwright.config.ts`'s port-3000 fallback
   (flagged as a tooling concern in round 25, not a product bug) is exactly
   that: a documented, already-mitigated (`E2E_PORT` wrapper script) test-only
   convenience. On the product side, `internal/server/server.go:53-74`'s
   `checkOrigin` validates against `s.port` — the port the listener *actually*
   bound to at runtime (recorded at `:103-105` from `ln.Addr()`, which resolves
   `host:0` correctly) — not a hardcoded value, so a live deployment has no
   equivalent port-uniqueness assumption baked into product code. The web
   client (`web/src/ws.ts:19`) derives its WS URL from `location.host`, which
   is likewise runtime-correct rather than a hardcoded port.
4. **Six under-audited large components read end to end** (via a focused
   sub-review): `TodoList.tsx`, `SubAgentBlock.tsx`, `ScopedModelsModal.tsx`,
   `ModelSelector.tsx`, `MCPSettings.tsx`, `ChatToolbar.tsx` (the last of which
   produced F-1 above). The other five, cross-checked against every prior
   round's fix history (tasks #663-#682) to rule out re-derivation:
   - `TodoList.tsx`: the `editStartContentRef` index-race guard (task #668) is
     in place and correct.
   - `SubAgentBlock.tsx`: `toolResultArrived`/`parentDone`/`errorFinish`
     (tasks #666, #669) verified intact; the lazy-load effect's `requested`
     ref latch is idempotent despite `parent` changing identity every stream
     tick.
   - `ScopedModelsModal.tsx`: all 4 session model-slot field names verified
     against `types.ts`; clamp-on-model-change logic correct in both row
     variants; the `scoped_models` broadcast listener is type-keyed so it
     also catches the server's own post-mutation broadcast without needing an
     extra active-session-change effect (backdrop-click-to-close makes a
     session switch while the modal is open unreachable anyway).
   - `ModelSelector.tsx`: the effort-clamp effect is dependency-complete and
     self-terminating; the "nil means untouched slot" convention matches the
     backend's handling.
   - `MCPSettings.tsx`: `MCPForm.submit()`'s reply listener is scoped per-
     `msgID` (task #667's fix) and detached on unmount, so concurrent Add/Edit
     forms cannot cross-wire replies.
5. **Go backend** — not reopened per the round-19 closure; item 2 above is the
   only backend code newly read this round, and it produced no reachable
   behavioral symptom (dead code, not a live bug).

## Reproduction artefacts

All outside the repository, under `D:\system_artefact\Temp\crush-r26\`. The
working tree was not modified (`git status --porcelain` shows only the
pre-existing `D web/dist/.gitkeep` and untracked `docs/`/`dev/` entries that
predate this session).

| path | what |
|---|---|
| `pw.config.ts` | Playwright config, `testDir ./spec`, `baseURL http://localhost:3131` (dev server) |
| `pw.prod.config.ts` | same specs, `baseURL http://localhost:3132` (production bundle) |
| `serve-dist.mjs` | read-only static server for the prebuilt `web/dist` (never writes) |
| `helpers/mock-ws.ts`, `helpers/fixtures.ts` | verbatim copies of the repo's own `web/tests/helpers/*` |
| `spec/system-prompt-error.spec.ts` | F-1: control (successful save) + two bug tests (error-after-click, stale-content-on-reopen) |

To re-run: `cd /d/system_artefact/Temp/crush-r26 && cmd //c "mklink /J node_modules D:\dev\go\crush\web\node_modules"`,
then `node_modules/.bin/playwright.CMD test --config=pw.config.ts` (dev server,
auto-started via the config's `webServer` block) and, separately,
`node serve-dist.mjs D:/dev/go/crush/web/dist 3132` followed by
`node_modules/.bin/playwright.CMD test --config=pw.prod.config.ts` (production).
