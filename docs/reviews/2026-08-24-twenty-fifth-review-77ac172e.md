# Round 25 review — `crush` fork @ `77ac172e`

**Verdict: NO-GO.** Two P2s: an attachment the operator visibly added is silently
dropped on two of the four send paths (the badge is even cleared on one of them,
so it reads as "sent"), and round 21's own follow-up fix `24e556f5` introduced a
StrictMode-unsafe `disposedRef` that makes the entire Providers editor a dead
button in `pnpm dev` — which is also why **12 of the repo's own 340 Playwright
tests are red on `main` right now**.

Round 24's four fixes were re-derived and independently re-executed; all four
hold, and the suggested angle (the terminal/non-terminal boundary in
`mergePreserveContent`) was traced end to end through `agent_turn.go` and
`message.Service.Update` and found closed. See *Swept, nothing filed* §1–§3.
Nothing round 24 fixed is re-flagged.

Every finding below was reproduced against BOTH the dev server and the
**production** bundle (`web/dist`, served read-only), each with a passing
control in the same run, so none of them is a harness or dev-mode artefact —
except F-2, which the production run specifically proves is dev-only *today*.

---

## F-1 (P2) — an attached file is silently discarded on the fast-send and queue paths; on fast-send the badge is cleared too, so it reads as "sent"

### Where

`web/src/components/ChatInput.tsx:341-351` — the "Send with lightweight model"
button's handler:

```tsx
  const sendFast = useCallback(() => {
    const msg = text.trim();
    if (!msg || !activeSessionID || agentBusy) return;
    sendWithFastModel(activeSessionID, msg);
    setText("");
    setAttachments([]);          // <- badge cleared: looks like it was sent
    setHistIdx(-1);
    setStash("");
    setHistoryOpen(false);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy]);
```

`web/src/store.ts:616-630` — `sendWithFastModel` builds the frame and has no
attachment parameter at all:

```ts
export function sendWithFastModel(sessionID: string, content: string) {
  …
  const payload: Record<string, unknown> = { sessionID, content };
  if (fastModel) {
    payload.smartModel = fastModel;
  }
  ws.send("send_message", payload);
}
```

Compare the ordinary idle path, `ChatInput.tsx:320-333`, which does carry them —
and whose `else` branch is the ONLY one of the four send paths that does:

```tsx
    if (agentBusy) {
      enqueueMessage(activeSessionID, msg);          // <- text only
    } else {
      const payload: Record<string, unknown> = { sessionID: activeSessionID, content: msg };
      if (attachments.length > 0) {
        payload.attachments = attachments.map((a) => ({ fileName: a.fileName, mimeType: a.mimeType, data: a.data }));
      }
      ws.send("send_message", payload);
      setAttachments([]);
    }
```

The queue half: `enqueueMessage(sessionID, content)` (`store.ts:666-670`) stores
`{ id, content }` — there is no field for an attachment — and the flush in
`web/src/useWS.ts:284-290` re-sends text only:

```ts
        if (!p.Busy) {
          const combined = dequeueAllMessages(p.SessionID);
          if (combined) {
            ws.send("send_message", { sessionID: p.SessionID, content: combined });
          }
        }
```

This is a pure client-side omission. The wire and the backend already accept
attachments alongside a per-call model override in the *same* frame —
`internal/server/protocol.go:147-158`:

```go
type SendMessagePayload struct {
	SessionID   string `json:"sessionID"`
	Content     string `json:"content"`
	Attachments []struct {
		FileName string `json:"fileName"`
		MimeType string `json:"mimeType"`
		Data     []byte `json:"data"`
	} `json:"attachments,omitempty"`
	SmartModel *ModelOverrideWire `json:"smartModel,omitempty"`
	FastModel  *ModelOverrideWire `json:"fastModel,omitempty"`
}
```

The two remaining send paths, `interrupt` (`:361-385`) and `inject` (`:393-417`),
both build the attachment array correctly — so this is 2 of 4, not a
deliberate global policy.

### Concrete failure scenarios

**(a) Fast-send — silent, and actively misleading.** Operator drags a screenshot
into the composer (badge appears: `screenshot.png (8 B)`), types "what is in this
image?", and clicks the ⇥ "Send with lightweight model" button — the natural
choice for a cheap one-shot visual question. The badge **disappears**, the text
clears, the message appears in the transcript. The model receives text only and
answers about nothing. There is no error, no toast, and — because the badge was
cleared — no residue anywhere in the UI to suggest the file did not go.

**(b) Queue-while-busy.** Agent is mid-turn. Operator attaches the file, types,
and clicks the button (which reads **Queue** while busy). The text lands in the
client-side queue; the badge stays on screen. When the turn ends, `useWS`
auto-fires the queued text with no attachment. The badge remaining is the only
signal, and it reads as "not sent yet" rather than "your file was detached from
the message you queued" — so the file then silently rides along with whatever
the operator sends *next*.

### Reproduction

`spec/fastsend-attachment.spec.ts` and `spec/queued-attachment.spec.ts`, live
against the real UI (all outside the repo). Run against the dev server AND the
production bundle with identical results:

```
  ok  C1: control — attachment IS sent when the agent is idle
      idle send payload: {"sessionID":"att-sess","content":"look at this",
        "attachments":[{"fileName":"screenshot.png","mimeType":"image/png","data":"iVBORw0KGgo="}]}

  x   C2: attachment is silently dropped when the same message is QUEUED while busy
      queue-flush payload: {"sessionID":"att-sess","content":"look at this"}
      attachment badge still visible after flush: true

  x   D: fast-send discards the attachment AND clears the badge
      fast-send payload: {"sessionID":"fast-sess","content":"what is in this image?",
        "smartModel":{"provider":"anthropic","model":"claude-haiku-4"}}
      attachment badge cleared (looks 'sent'): true
```

C1 is the control that rules out a harness artefact: the *same* file, set through
the *same* `input[type=file]`, in the *same* run, reaches the wire on the idle
path.

### Fix direction (not applied — read-only review)

Give `sendWithFastModel` an `attachments` parameter and build the same array the
idle branch does; give `QueuedMessage` an `attachments` field and have the
`agent_busy(false)` flush re-attach them (the flush already concatenates N queued
texts, so it needs to concatenate N attachment arrays too). Whichever is done
first, the *other* path must at minimum stop clearing `setAttachments([])` when
it did not actually send them.

---

## F-2 (P2) — `24e556f5`'s `disposedRef` latches `true` under React StrictMode, so the whole Providers editor is a dead button in `pnpm dev` — and 12 of the repo's own e2e tests are red on `main`

### Where

`web/src/components/ProvidersModal.tsx:132-141` (`ProviderForm`) and
`:408-415` (`BuiltinProviderEditor`) — both added by `24e556f5`:

```tsx
  const unsubRef = useRef<(() => void) | null>(null);
  // Guards the dynamic import in submit(): if the form unmounts before the
  // import resolves, the .then must not register a handler at all.
  const disposedRef = useRef(false);
  useEffect(() => {
    return () => {
      disposedRef.current = true;
      unsubRef.current?.();
    };
  }, []);
```

The effect body is **empty**; nothing ever sets `disposedRef.current` back to
`false`. `web/src/main.tsx:12-16` wraps the app in `<StrictMode>`:

```tsx
createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>
);
```

React's development build double-invokes mount effects — run, **cleanup**, run
again — on the *same* fiber, so the same `useRef` object survives. After mount
the flag is therefore permanently `true`, and both submit paths bail before
`ws.send` is ever reached:

`ProvidersModal.tsx:166-186`:

```tsx
  function submit() {
    const err = validate();
    if (err) { setError(err); return; }
    setError(null);
    setBusy(true);                                   // <- spinner turns on
    const msgID = crypto.randomUUID();
    import("../ws").then(({ ws }) => {
      if (disposedRef.current) return;               // <- always true; returns here
      …
      onSubmit({ … }, msgID);                        // never reached
    });
  }
```

`:419-434` (`sendAndWait`) has the identical `if (disposedRef.current) return;`.
`MCPSettings.tsx:104-106` and `SettingsModal.tsx:90-92,155-157` got the same
commit's `unsubRef` half but **no** `disposedRef`, so they are StrictMode-safe —
`ProvidersModal.tsx` is the only affected file.

### Concrete failure scenario

Anyone running the documented dev workflow (`cd web && pnpm dev`): open
**More → Providers**, edit a custom provider or add one, click **Update
Provider** / **Add Provider**. The button flips to a spinner reading `Saving…`
(`:369-370`) and stays there forever. No frame is sent, no error is shown, the
form never closes. Same for the built-in-provider editor's API-key save/remove
and peak-hours save (`:573-576`). Every mutating control in that modal except
"Remove provider" (which goes through the unguarded `removeCustomProvider`) is
dead.

**And this is why the project's own e2e suite is red.** `web/playwright.config.ts`
starts `webServer: { command: "npm run dev" }`, i.e. the StrictMode-double-
invoking dev build, so the suite exercises exactly the broken path. Running the
repo's own `web/tests` against a dev server on `main` @ `77ac172e`:

```
  12 failed
    providers.spec.ts:146  Edit provider sends update_custom_provider
    providers.spec.ts:200  Add provider form with model saves correctly
    providers.spec.ts:288  Add provider form sends peakHours when enabled
    providers.spec.ts:307  Add provider form omits window when peak-hours disabled
    providers.spec.ts:322  Add provider form defaults to global scope
    providers.spec.ts:336  Add provider form sends local scope when selected
    providers.spec.ts:366  Edit provider clears peak-hours when toggled off
    providers.spec.ts:467  Built-in provider peak-hours editor sends set_provider_peak_hours
    providers.spec.ts:485  Built-in provider peak-hours editor can target local scope
    providers.spec.ts:502  Built-in provider editor sends set_provider_key when a key is entered
    providers.spec.ts:528  Built-in provider editor Remove button sends remove_provider_key
    settings.spec.ts:168   Providers modal - add custom provider sends command
  328 passed (5.5m)
```

Every one of those 12 asserts a WS frame from a `disposedRef`-guarded path; every
DOM-only providers test passes. `.githooks/pre-push` runs only `go build`,
`golangci-lint` and `go test` — it never runs the web suite, which is how this
went unnoticed since `24e556f5` landed at 01:32 today.

### Scope: production is not affected TODAY

React only double-invokes effects in its development build, so the embedded
bundle (`//go:embed all:dist`, built by `rsbuild build`) does not latch the flag.
Verified by differential, not assumed — the *same* probe file run against the
prebuilt production bundle served read-only on `:3127`:

```
  /ws sockets constructed on one page load: 1          (dev server: 2)
  frames after clicking Update Provider:
    [{"type":"update_custom_provider","payload":{"oldId":"ollama","id":"ollama",
      "name":"Ollama Local", … ,"scope":"global"},"id":"ef2105d9-…"}]
  ok  E1   ok  E3 (control)
```

and the repo's own two spec files against the same production bundle:

```
$ playwright test --config=pw.repo.prod.config.ts providers.spec.ts settings.spec.ts
  46 passed (40.1s)
```

The `__wsCtorCount` probe (E0) is the mechanism proof: `useWS`'s single mount
effect calls `ws.connect()` exactly once per invocation, and the page constructs
**2** `/ws` sockets on the dev server vs **1** on the production bundle.

That said, "dev-only" is a property of today's React, not of the code: the
pattern is unsound by React's own contract — `<Activity>`/Offscreen remounts
re-run effects while preserving state *and refs*, which is precisely what
StrictMode's double-invoke exists to simulate. This is a latent production bug,
not a test-harness quirk.

### Fix direction

One line in each of the two components: reset the flag at the top of the effect
body instead of relying on it never having been set —

```tsx
  useEffect(() => {
    disposedRef.current = false;          // <- add
    return () => {
      disposedRef.current = true;
      unsubRef.current?.();
    };
  }, []);
```

`unsubRef`/`pendingUnsubs` need no change (their cleanup is idempotent on the
double-invoke). Worth adding a permanent spec assertion that the *submit* path
emits its frame, not just that the form renders — the 12 tests that already do
this are the ones that caught it, and they only failed because the suite runs
against `dev`.

---

## F-3 (P3) — the sidebar's session-rename **Cancel** button commits the rename

### Where

`web/src/components/Sidebar.tsx:164-173` — the edit input saves on blur:

```tsx
                      <input
                        ref={inputRef}
                        value={editTitle}
                        onChange={(e) => setEditTitle(e.target.value)}
                        onBlur={saveRename}
                        onKeyDown={handleKeyDown}
                        data-test-id="session-edit-input"
                        …
                      />
```

`:183-190` — and the Cancel button sits right next to it:

```tsx
                        <button
                          onClick={(e) => { e.stopPropagation(); setEditingID(null); }}
                          title="Cancel (Esc)"
                          data-test-id="session-edit-cancel"
                          …
                        ><X size={12} /> Cancel</button>
```

`mousedown` on Cancel blurs the still-mounted input **before** its `click` fires,
so `saveRename` (`:71-76`) runs first and sends `rename_session`. The Escape path
(`handleKeyDown` → `setEditingID(null)`) unmounts the input without a `focusout`
and is correct — which is what makes this a slip rather than a design choice.

### Concrete failure scenario

Operator double-clicks a session title, types a new name, then decides against it
and clicks the button explicitly labelled **Cancel**. The session is renamed
anyway. The title in the sidebar changes to the abandoned draft, and there is no
undo.

### Reproduction

`spec/sidebar-rename-cancel.spec.ts` (dev and production, identical):

```
  x   B1: clicking Cancel on a session rename must NOT send rename_session
      frames after clicking Cancel:
        [{"type":"rename_session","payload":{"sessionID":"rn-1","title":"TYPO-I-DID-NOT-MEAN-THIS"}}]
  ok  B2: control — pressing Escape correctly does NOT send rename_session
      frames after Escape: []
  ok  B3: control — clicking Save DOES send rename_session
```

B2 and B3 bracket the defect: the same form, same locator, same run — Escape
correctly sends nothing and Save correctly sends the rename.

### Fix direction

Either guard `saveRename` behind an intent flag the Cancel button clears on
`onMouseDown` (before blur), or drop `onBlur={saveRename}` entirely — the row
already has explicit Save/Cancel buttons plus Enter/Escape, so blur-to-save is
redundant. (`TodoList.tsx:203`'s `onBlur={commit}` is *not* the same shape: that
row has no Cancel button, so blur-commits there is unambiguous.)

---

## F-4 (Minor) — `ActionRow`'s burst thinking row still stashes its edit form across a collapse; `55d32c4d` added that safeguard to `ThinkingPart` but `2c761d8d` did not port it

### Where

`web/src/components/Message/ActionRow.tsx:83` — the row toggle only flips the
open override:

```tsx
  const toggle = useCallback(() => setOverride(!open), [open]);
```

`:86` — `editingThinking` is independent state that nothing clears on collapse:

```tsx
  const [editingThinking, setEditingThinking] = useState(false);
```

`:148-164` — the body (and therefore the `EditForm`) is unmounted by the `open`
guard, but the component itself stays mounted, so the flag survives:

```tsx
        {open && (
          <div className="action-row-body">
            {editingThinking ? ( … <EditForm … /> … ) : (
              <pre className="tool-output whitespace-pre-wrap">{item.text}</pre>
            )}
          </div>
        )}
```

`ThinkingPart.tsx:25,34-37` is the corrected counterpart `55d32c4d` shipped —
its commit message calls this out explicitly as "an anti-stash safeguard beyond
what the review asked" — while `2c761d8d` fixed only the swallowed-click half in
`ActionRow`:

```tsx
  const collapse = useCallback(() => { setOpen(false); setEditing(false); }, []);
  …
  const toggleOpen = useCallback(() => {
    if (open) collapse();
    else setOpen(true);
  }, [open, collapse]);
```

### Concrete failure scenario

Operator clicks Edit on a burst thinking row (it auto-expands — `2c761d8d`
works), changes their mind, and collapses the row with its own chevron instead of
pressing Cancel. Later they click that row open to *read* the reasoning and get
an edit textarea instead — pre-filled from `item.text`, so their draft is gone
too. They must press Cancel to see the content they came for. Not destructive
(no WS frame is sent), which is why this is Minor and not the P2 its
`ThinkingPart` sibling was.

### Reproduction

`spec/actionrow-stash.spec.ts` (dev and production, identical):

```
  x   A: collapsing a burst thinking row mid-edit stashes the edit form;
         re-expanding to READ shows a textarea
      after re-expand: textareas = 1  pre blocks = []
  ok  B: control — ThinkingPart's standalone Thoughts card does NOT stash (55d32c4d)
```

Test B is the control that makes the asymmetry the finding: the identical
sequence on the *other* renderer of the same affordance behaves correctly.

### Fix direction

Mirror `ThinkingPart`: give `ActionRow` a `collapse` that clears
`editingThinking` alongside `setOverride(false)`, and route the header toggle
through it.

---

## Swept, nothing filed

So round 26 does not re-walk this ground.

1. **Round 24's four fixes re-derived and re-executed — all four hold.**
   - `77ac172e` (F-1, the merge guard): `store.ts:206-208`'s
     `if (isTerminallyFinished(incoming.Parts)) return incoming;` is in place and
     the import at `:358` is real. Its five permanent tests in
     `web/tests/merge-guard.spec.ts` pass in this round's own independent run.
   - `55d32c4d` (F-2): `ThinkingPart.tsx`'s `ConfirmDialog` (`:96`) and `EditForm`
     (`:109`) are both outside the `open` guard, `openEditEv` forces open
     (`:33`), and `collapse` clears `editing` (`:25`). Re-verified live by this
     round's own control test B (see F-4).
   - `2c761d8d` (the ActionRow sibling): the Edit button does
     `setEditingThinking(true); setOverride(true);` (`:127-128`) and the row
     genuinely auto-expands — re-verified live (F-4's step 1). *(F-4 is the
     second half of that same bug, which the commit deliberately did not
     address.)*
   - `9cd0485d` (F-3, effort badge): `BurstPart` carries `model`/`effort`
     (`Chat.tsx:147,203,234`), `ToolRun` threads them (`:265`),
     `ToolActivityGroup` falls back per-row (`:229-230,266-267`).
   - All 340 repo web tests were executed this round; the only failures are
     F-2's 12, none of which is in the round 22–24 regression set.
2. **The suggested angle — can a mid-stream `message_updated` still trip the
   guard, or is the terminal boundary gameable?** Traced end to end; **no**.
   - *Terminal → non-terminal on the wire is blocked at two layers.*
     `AddFinish` (`internal/message/content.go:501-511`) deletes any existing
     Finish before appending, so a late checkpoint snapshot genuinely can carry a
     `Partial` Finish over a terminal one — but `message.Service.Update`
     (`internal/message/message.go:399-414`) routes every `Partial` write through
     `UpdateMessageIfNotTerminal` (fenced on `finished_at IS NULL` +
     `checkpoint_generation`), and `:462-470` publishes **only** when
     `rowsAffected > 0`. A stale partial therefore reaches neither the DB nor the
     socket.
   - *A finish-less snapshot can't undo a terminal row either.* Those take the
     unconditional `UpdateMessage` branch, but every producer of one
     (`OnReasoningStart/End`, `OnToolInputStart/End`, `OnToolCall` —
     `agent_turn.go:1138,1170,1215,1230,1269`) clones `currentAssistant`, which
     retains its Finish from `OnStepFinish` onwards, and each step gets a *new*
     message via `PrepareStep` (`:1113,1127`).
   - *The 20fps `Notify` ticker cannot publish a stale snapshot after the terminal
     write.* `OnStepFinish` orders it explicitly: `stopCheckpoint()` (`:1313`) →
     `AddFinish` (`:1362-1381`) → drain `latestMsgCh` (`:1383-1388`) → terminal
     `a.messages.Update` (`:1522`). The drain precedes the terminal write, and no
     streaming callback runs concurrently with `OnStepFinish`.
   - *Inside the narrowed window the guard's own logic is still length-only, and
     that is now correct*: with the terminal case removed, the only remaining
     input is a genuinely mid-stream re-broadcast, where a shorter snapshot IS
     stale — and `updateMessageAndVerify` (`handlers_messages.go:70-78`) refuses
     every part-level mutation on such a row, so a rebuilt (`[thinking…, text…,
     advancing…]`) client array cannot be committed against. The commit message's
     reasoning for deliberately NOT adding a part-count exemption checks out.
   - **One residual, deliberately not filed** because I could not demonstrate its
     trigger: the re-sync depends on the terminal `message_updated` actually
     arriving. `hub.Run`'s fan-out drops on a full client buffer
     (`internal/server/hub.go:467-473`, `select … default:`), and a locally-owned
     session is never polled (`useWS.ts:331-337` only refetches `OwnedExternal`).
     A dropped terminal broadcast *after* a mid-stream regression rebuild would
     leave the client's `Parts` order diverged against a row the server now
     considers terminal — which is round 24 F-1(b)'s escalation via a different
     door, since `ActionRow`/`IntermediateAssistantMessage` have no terminal gate
     on their Edit/Delete. Requires a stalled socket at exactly the terminal
     write; I could not produce one. Recorded, not claimed.
3. **Hub replay on WS reconnect** — checked as the obvious second way to feed the
   guard a stale snapshot, and ruled out. `hub.Run`'s register case replays the
   whole ring (`hub.go:443-449`) to a reconnecting client whose `$messages` was
   NOT cleared (`useWS.ts:74-89` does not reset it, and the `sessions_list`
   handler skips `load_messages` when `activeID === hashID`), so an old
   `message_created` with `Parts: []` does briefly rebuild a live message. But the
   ring evicts oldest-first (`:307-313`), so the LAST replayed event for every
   message is its newest one, and after `77ac172e` a terminal newest is taken
   verbatim — the replay converges. Not filed.
4. **Every UI→server index/ID derivation re-audited** (the class rounds 23–24
   kept finding). All five part-mutating call sites derive their `partIndex` from
   a real `Parts` index: `ActionRow` ← `BurstPart.partIndex` ← `Chat.tsx:203,234`;
   `ThinkingPart` ← `Part.tsx:21` ← `blocks.ts:38`; `IntermediateAssistantMessage`
   ← `Chat.tsx:239`; `AssistantContent.tsx:104` passes `partIndex: it.idx`.
   `ActionItem`'s `kind:"tool"` variant carries no `partIndex` at all
   (`ActionRow.tsx:44-55`) and tool rows expose no edit/delete, so
   `ToolActivityGroup`'s consecutive-duplicate dedup (`:155-174`) cannot
   mis-address anything.
5. **`updateMessageThinking`'s missing index** — `ThinkingPart.tsx:40` sends
   `update_message_thinking` (no index) for Edit while `:45` sends
   `delete_message_part(partIndex)` for Delete, and the server
   (`handlers_messages.go:295-310`) rewrites the FIRST `ReasoningContent`
   regardless of index. Chased and **not filed**: a message can never hold two
   reasoning parts, because `Message.AppendReasoningContent`
   (`content.go:317-337`) appends the delta to *every* existing one and creates at
   most one; `AppendReasoningSignature` / `AppendThoughtSignature` /
   `SetReasoningResponsesData` all `return` on the first. The two commands
   therefore always address the same part. Worth knowing the asymmetry exists if
   multi-reasoning-part messages ever become possible.
6. **`handleUpdateMessageContent` merges N text parts into one**
   (`handlers_messages.go:262-276`) — the UI feeds it `extractText(message.Parts)`
   (all text parts joined), so content is preserved even though structure is not;
   and a tool-less assistant message (the only shape routed to `<Message>` by
   `buildRenderItems`) can only ever have one text part anyway, since
   `AppendContent` (`content.go:304-315`) opens a new part only after a non-text
   part. Not filed.
7. **`setSessionReasoningEffort`'s both-slots serialisation** (`store.ts:438-474`)
   — it always sends BOTH `smartModel` and `fastModel`, which per
   `clearSessionModelSlot`'s own doc is "wipe this override" when
   provider/model are empty. Traced and ruled out: the empty case only occurs on
   a slot that has no override to wipe, and `handleSetSessionModels`
   (`handlers_models.go:95-109`) already back-fills the unset effort from the DB
   before writing, with a comment naming exactly this hazard.
8. **`AskQuestionBlock` reachability** — the awaiting-answer finish uses
   `FinishReasonError` (`agent_turn.go:1762`), so `buildRenderItems`'
   `hasErrorFinish` branch (`Chat.tsx:217-221`) routes the message to a standalone
   `<Message>` even when it carries the `ask_question` tool_call, and the block
   renders. Fine.
9. **`Message.tsx`'s stream gates** (`:49-80`) hold: `editable`/`deletable`/
   `selectable` all key off `isTerminallyFinished` with the documented idle-orphan
   exception. `SummaryMessage.tsx:41` gates its Edit on the same predicate, and
   the compaction summary does get a terminal `AddFinish`
   (`agent_compaction.go:445,628`) — so round 24 F-1's fifth affordance
   ("compaction summary → Edit") is genuinely covered by `77ac172e`'s gate, not
   just the thinking rows its tests exercise.
10. **`ConfirmDialog`'s global `Enter` → `onConfirm`** (`ConfirmDialog.tsx:23-30`,
    a `document` listener with no focus guard) — noted, not filed: it is
    consistent across every confirm site and the dialog is a full-screen overlay,
    so there is no competing focused control while it is up.
11. **Go backend** — not re-opened per the round-19 closure beyond the read-only
    tracing in §2, which produced no behavioural bug report. All four findings are
    frontend-only.

## Reproduction artefacts

All outside the repository, under `D:\system_artefact\Temp\crush-r25\`. The
working tree was not modified (`git status --porcelain` shows only the
pre-existing `D web/dist/.gitkeep` and untracked `docs/`).

| path | what |
|---|---|
| `pw.config.ts` | Playwright config, `testDir ./spec`, `baseURL http://localhost:3125` (dev server) |
| `pw.prod.config.ts` | same specs, `baseURL http://localhost:3127` (production bundle) |
| `pw.repo.config.ts` / `pw.repo.prod.config.ts` | the REPO's own `web/tests` against dev / production, with `outputDir` pinned outside the repo |
| `serve-dist.mjs` | read-only static server for the prebuilt `web/dist` (never writes) |
| `spec/fastsend-attachment.spec.ts` | F-1(a): fast-send drops the file and clears the badge |
| `spec/queued-attachment.spec.ts` | F-1(b): queued-while-busy drops the file; includes the idle-path control |
| `spec/providers-disposed.spec.ts` | F-2: E0 socket-count mechanism proof, E1 dead submit, E3 unguarded-path control |
| `spec/sidebar-rename-cancel.spec.ts` | F-3: Cancel commits; Escape + Save controls |
| `spec/actionrow-stash.spec.ts` | F-4: ActionRow stashes; ThinkingPart control |

To re-run: start the dev server (`cd web && PORT=3125 ./node_modules/.bin/rsbuild dev`)
and the prod server (`node serve-dist.mjs D:/dev/go/crush/web/dist 3127`), recreate the
module link with
`cd /d/system_artefact/Temp/crush-r25 && cmd //c "mklink /J node_modules D:\dev\go\crush\web\node_modules"`,
then `npx playwright test --config=pw.config.ts` (and `--config=pw.prod.config.ts`).
