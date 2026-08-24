# Round 24 review — `crush` fork @ `8aa89dca`

**Verdict: NO-GO.** One P1 — every *successful* part-level edit or delete on an
assistant message is invisible in the UI, because `store.ts`'s anti-flicker
regression guard cannot tell a deliberate shrink from a stale re-broadcast; the
client's `Parts` array then diverges from the DB's and a repeat click on the
still-visible row deletes a **different** part, with nothing on screen ever
changing. Reproduced end to end (real `store.ts`, real WS handlers, real UI).
One P2 — the standalone "Thoughts" card's Edit/Delete buttons are clickable
while the card is collapsed but their dialogs are rendered inside an `{open &&}`
guard, so the click is swallowed and the *destructive confirm dialog* fires
later, unprompted, the moment the operator expands the card to read it.

All four round-23 fixes (`ddf931ee`, `c1c85d4c`, `789671e9`, `8aa89dca`) were
re-derived from the code and hold — see *Swept, nothing filed* §1. Nothing they
fixed is re-flagged.

---

## F-1 (P1) — a successful `delete_message_part` / `update_message_part` never reaches the screen, and a second click deletes the wrong part

### Where

`web/src/store.ts:192-221` — the guard, verbatim:

```ts
export function mergePreserveContent(existing: Message, incoming: Message): Message {
  const existingThinking = totalThinking(existing.Parts);
  const incomingThinking = totalThinking(incoming.Parts);
  const existingText = totalText(existing.Parts);
  const incomingText = totalText(incoming.Parts);

  const thinkingRegressed = incomingThinking.length < existingThinking.length;
  const textRegressed = incomingText.length < existingText.length;

  if (!thinkingRegressed && !textRegressed) {
    return incoming;
  }

  const thinkingParts = thinkingRegressed
    ? partsOfKind(existing.Parts, "thinking")     // <- keeps the DELETED part
    : partsOfKind(incoming.Parts, "thinking");
  const textParts = textRegressed
    ? partsOfKind(existing.Parts, "text")         // <- keeps the DELETED part
    : partsOfKind(incoming.Parts, "text");
  const advancingParts = incoming.Parts.filter(
    (p) => p.type !== "thinking" && p.type !== "text",
  );

  return { ...incoming, Parts: [...thinkingParts, ...textParts, ...advancingParts] };
}
```

`web/src/store.ts:223-248` (`upsertMessage`) routes **every** assistant→assistant
`message_updated` through it; `web/src/useWS.ts:182-194` is the only consumer of
that event. Its doc comment (`store.ts:165-191`) states the premise the rule
rests on — *"occasionally a stale snapshot arrives … whose thinking OR text is
shorter than what the user was just shown"*. Length is the **only**
discriminator, and a deliberate shrink is length-indistinguishable from a stale
snapshot.

The five UI affordances that produce exactly such a shrink, none of which does
an optimistic local update or a refetch — the server broadcast is their only
path to the screen:

| affordance | call site | WS command |
|---|---|---|
| burst thinking row → Edit / Delete | `ActionRow.tsx:142`, `:156` | `update_message_part` / `delete_message_part` |
| "Thoughts" card → Edit / Delete | `ThinkingPart.tsx:30`, `:35` | `update_message_thinking` / `delete_message_part` |
| intermediate narration → Edit / Delete | `IntermediateAssistantMessage.tsx:39`, `:46` | `update_message_part` / `delete_message_part` |
| assistant message → Edit | `Message.tsx:95` | `update_message_content` |
| compaction summary → Edit | `SummaryMessage.tsx:26` | `update_message_content` |

Server side the write is real and durable: `handlers_messages.go:321-351` /
`:353-399` mutate `m.Parts` and commit through `updateMessageAndVerify`
(`:53-87`, which only returns true for a terminally finished row), and
`message.Service.Update` (`internal/message/message.go:377-486`) publishes the
new row via `PublishMustDeliver` at `:484` → `events.go:78-80,60-65` →
`EventMessageUpdated`.

### Concrete failure scenarios

Take the shape `agent_turn.go` writes for one step that reasons, narrates, then
calls a tool — `Parts = [thinking, text, tool_call, finish]`, which
`Chat.tsx:222-227`'s own comment calls the normal case ("A text part is the
model narrating between actions").

**(a) Delete is invisible.** Operator opens the thinking row, clicks the trash,
confirms. The server deletes `Parts[0]` and broadcasts `[text, tool_call,
finish]`. `mergePreserveContent` sees incoming thinking 0 chars < on-screen 57
chars, calls it a regression, and re-inserts the deleted part. **The reasoning
the operator just deleted is still on screen**, and stays there until a session
switch or reload (a locally-owned session is never polled — `useWS.ts:331-337`
only refetches messages for `OwnedExternal` sessions).

**(b) The second click deletes something else.** Because the row still looks
identical, clicking Delete again is the natural response. The UI re-derives
`partIndex` from the **client's** `Parts` array, so it sends `partIndex: 0`
again — server-side that is now the `TextContent`, which passes the round-23
delete allow-list (`handlers_messages.go:339-345` permits `TextContent,
ReasoningContent`). The agent's narrating text is removed from the transcript —
and from the provider history replayed on every later turn — with nothing on
screen having changed at any point. A third click finally errors ("part type not
deletable", `tool_call`), which is the operator's first feedback of any kind, and
it is about the wrong thing.

**(c) Edits silently revert.** Editing a thinking row (or an assistant answer,
or a compaction summary) to anything *shorter* leaves the pre-edit text on
screen. The DB holds the new text; reopening the editor shows the old one. The
operator's reasonable conclusion is "the edit didn't take", so they edit again —
each attempt lands server-side and none is ever displayed.

**(d) Client/DB part order diverges.** The rebuild's fixed
`[thinking…, text…, advancing…]` order is not the DB's order, so from the first
regression onwards every `partIndex` the UI derives is an index into an array
the server does not have. This is the same failure mode round 23's F-1 fixed one
layer up (burst position used as a part index); that fix threaded the *real*
index through the render pipeline, but the array it indexes into can itself be
wrong.

### Why this survived round 20's sweep

Round 20's report (`docs/reviews/2026-08-24-twentieth-review-9c1d661b.md:429`)
examined exactly this and closed it:

> **Client-side part reordering vs. `partIndex`** … The rebuild only triggers
> during streaming, and `updateMessageAndVerify` refuses every write to a
> still-streaming assistant message, so a mismatched index can never be
> committed. Once the turn is terminal the client's order re-syncs (no
> regression → `return incoming` verbatim). Not filed.

The premise "the rebuild only triggers during streaming" is false. A successful
part edit/delete on a **terminal** message is itself a regression by the guard's
only test, so the rebuild triggers precisely where round 20 assumed it could
not, and `updateMessageAndVerify` — which gates on *streaming*, not on index
sanity — waves the second delete straight through.

### Reproduction

Three independent harnesses, all outside the repository; the working tree was
not modified (`git status --porcelain` unchanged apart from pre-existing
untracked docs).

**(a) Over the real `store.ts`, no slicing.** `merge_harness.mjs` transpiles
`web/src/store.ts` with the repo's own TypeScript (type erasure to CommonJS
only), loads it with a three-entry module shim (`nanostores`, `./ws`,
`./telemetry`) plus the `localStorage` bootstrap it touches at module scope, and
calls the **real** `upsertMessage`:

```
=== A. delete_message_part on the thinking part (server succeeded) ===
  client before                      [thinking, text, tool_call, finish]
  server broadcast                   [text, tool_call, finish]
[upsertMessage] content regression blocked { id: 'a1', prevThinking: 57, incomingThinking: 0, … }
  client after                       [thinking, text, tool_call, finish]
  BUG REPRODUCED  deleted thinking part is still rendered — client keeps 4 parts, DB has 3
  BUG REPRODUCED  a second Delete click addresses the wrong part server-side
                  — client sends partIndex 0; server Parts[0] is a "text"

=== B. update_message_part shortening a thinking part ===
  BUG REPRODUCED  edited thinking reverts to the pre-edit text
                  — UI shows "I should look at the con…", DB holds "shorter"

=== C. update_message_content shortening an assistant answer ===
  BUG REPRODUCED  edited answer text reverts to the pre-edit text

=== D. control: a LONGER edit (no regression) applies normally ===
  ok

=== E. control: part ORDER after the guard rebuilds ===
  BUG REPRODUCED  deleted narrating text is still rendered

5 of 6 checks reproduced the defect.
```

**(b) Over the real WS handlers.** `zz_r24_probe_test.go` + `overlay.json`,
injected into `package server` via `go test -overlay` (nothing written into the
repo), plays the two clicks against `handleDeleteMessagePart` and prints the
actual `toMessageWire` payload that feeds harness (a):

```
$ go test -C D:/dev/go/crush -overlay=…/overlay.json ./internal/server/ \
    -run TestZZR24DoubleDeleteProbe -v -count=1
    initial DB parts          = [thinking text tool_call finish]
    after click 1, DB parts   = [text tool_call finish]
    message_updated payload   = {"ID":"27d8…","Role":"assistant",…,"Parts":[
      {"type":"text","Text":"OK, the file exists, now let me edit it"},
      {"type":"tool_call","ID":"tc1","Name":"edit","Input":"{\"file_path\":\"a.ts\"}","Finished":true},
      {"type":"finish","Reason":"end_turn"}],…}
    second delete reply       = type="response" error=""
    after click 2, DB parts   = [tool_call finish]
    RESULT: the agent's narrating text part is gone from the transcript
--- PASS: TestZZR24DoubleDeleteProbe (0.55s)
```

**(c) Live, against the real UI.** `spec/merge-guard.spec.ts` drives the real
rsbuild dev server (port 3112) through the repo's own
`web/tests/helpers/mock-ws.ts` / `fixtures.ts`, injecting the exact wire payload
harness (b) printed:

```
  ok  A: deleting a thinking row leaves it on screen, and a second click deletes the wrong part
  ok  B: editing a thinking row to shorter text silently reverts on screen
  ok  C: deleting the agent's narrating text leaves it on screen too
  ok  D: control — a LONGER replacement does reach the screen
```

Test D is the control that rules out a harness artefact: an update that does
**not** trip the guard is applied to the DOM by the same code path in the same
run.

### Fix direction (not applied — read-only review)

The guard needs a signal other than length. Two shapes, in increasing order of
correctness:

1. **Cheapest:** have every part-mutating store action mark the message ID as
   "operator-edited" (a `Set<string>` cleared on the next `message_updated` for
   that ID) and have `upsertMessage` take `incoming` verbatim while the mark is
   set. Narrow, no protocol change, but racy against a concurrent stream.
2. **Correct:** the regression guard exists only for *streaming* re-broadcasts,
   and the wire already carries the discriminator — `Finish.Partial`
   (`wire.go:38`, read by `textParts.ts:24` as `isTerminallyFinished`). Apply
   `mergePreserveContent` only when the **incoming** message is not terminally
   finished; a terminal row is by definition not a mid-stream snapshot, and
   `updateMessageAndVerify` already refuses to commit edits to anything else.
   That closes the whole class — deletes, edits, and the part-order divergence —
   with one predicate.

Worth doing regardless of which: a `message_updated` whose part **count**
shrinks is never a stale-snapshot case the guard was written for, so at minimum
`thinkingRegressed`/`textRegressed` should not fire when the incoming message has
fewer parts of that kind than the existing one.

---

## F-2 (P2) — "Thoughts" card: Edit/Delete are clickable while collapsed, but their dialogs are gated on `open`, so the click is swallowed and the destructive confirm ambushes the operator later

### Where

`web/src/components/Message/ThinkingPart.tsx:72-76` — the buttons live in the
collapsed header, inside a `hover-reveal` strip that stops propagation (so the
click does **not** expand the card):

```tsx
        <div className="ml-auto flex items-center gap-0.5 hover-reveal" onClick={(e) => e.stopPropagation()}>
          <CopyButton text={thinking} className="px-1.5 py-1 text-xs" />
          <button onClick={openEditEv} title="Edit thinking"   className="btn-icon-sm"><Pencil size={13} /></button>
          <button onClick={openDel}    title="Delete thinking" className="btn-icon-sm-danger"><Trash2 size={13} /></button>
        </div>
```

`web/src/index.css:224` — `hover-reveal` is opacity-only, not
`pointer-events-none`, so the buttons are genuinely hit-testable on hover:

```css
  .hover-reveal { @apply opacity-0 group-hover:opacity-100 transition-opacity; }
```

`web/src/components/Message/ThinkingPart.tsx:81-99` — but both consumers of the
state those buttons set are inside the `open` guard, and `open` starts `false`
(`:21`, `useState(false)`):

```tsx
      {open && confirmDelete && (
        <ConfirmDialog
          title="Delete thinking"
          …
      {open && (editing ? (
        <div className="p-4 bg-base-overlay border-t border-surface">
          <EditForm …
```

The contrast that makes this a slip rather than a design choice: the *other*
renderer of the same affordance, `ActionRow.tsx:151`, renders its `ConfirmDialog`
**outside** any open-state guard — `{confirmDeleteThinking && (…)}` — and works
correctly whether the row is expanded or not.

### Concrete failure scenario

1. Operator hovers a collapsed "Thoughts" card, clicks the trash. Nothing
   happens: no dialog, no WS frame, no toast. The affordance looks broken.
2. Later — possibly much later, possibly for an entirely unrelated reason —
   they click the card header to *read* the reasoning. Instead of the reasoning,
   a **"Delete thinking — this cannot be undone"** confirmation dialog appears,
   unprompted, over a card they only meant to open.
3. Same for Edit: expanding the card afterwards shows an edit textarea where the
   reasoning should be (`thinking-content` is not rendered at all in that
   branch), so the operator cannot read what they came to read.

Because the card renders collapsed by default, step 1 is the *first* interaction
any operator has with these two buttons.

### Reproduction

`spec/thinkingpart-collapsed.spec.ts`, live against the real UI:

```
  ok  E: Delete on a COLLAPSED Thoughts card does nothing, then fires on expand
  ok  F: Edit on a COLLAPSED Thoughts card does nothing, then replaces the body on expand
  ok  G: control — the same buttons work once the card is expanded
```

E asserts both halves: after the click there is no `.modal-panel` and zero
`delete_message_part` frames in `window.__wsSent`; after a subsequent click on
`thinking-toggle` the "Delete thinking" dialog is visible. G is the control
proving the buttons and the locator are correct.

### Fix direction

Move both blocks out of the `open` guard, matching `ActionRow.tsx:151` — the
`ConfirmDialog` is a fixed overlay and does not need the body mounted; the
`EditForm` should force `setOpen(true)` when `openEditEv` fires (editing a
collapsed card implies opening it).

---

## F-3 (Minor) — the effort badge round 23 put on the wire still never renders on the dominant path

`web/src/components/Chat.tsx:279` renders the cross-message burst without
`model` / `effort`:

```tsx
        <ToolActivityGroup items={items} live={isLive} isCurrent={isCurrent} startedAt={startedAt} />
```

while the other caller, `AssistantContent.tsx:104`, passes both:

```tsx
            <ToolActivityGroup items={…} live={isLive} isCurrent={…} model={message.Model} effort={message.ReasoningEffort} />
```

`ToolActivityGroup` forwards them to every `ActionRow`, whose thinking header
has slots for both (`ActionRow.tsx:105-106`). Since `buildRenderItems` sends
*every* tool-bearing assistant message into a burst and never emits a `<Message>`
row for it, a long agentic turn shows neither the model name nor the effort tier
anywhere until its final prose message. That is precisely the capability
`c1c85d4c` was written to restore — `EffortBadge`'s own doc comment: *"operators
routinely run GLM at high vs max and want to tell them apart at a glance."*

Reproduced live (`spec/effort-in-burst.spec.ts`, 2 tests): with
`ReasoningEffort: "max"` genuinely on the wire, the badge and the model name are
absent from a burst thinking row and present on a tool-less assistant message in
the same session.

Minor because the information is still reachable by hovering the turn's final
assistant message (`AssistantHoverActions.tsx:59`). Note for whoever fixes it: a
burst spans N messages that may legitimately differ in model/effort, so the right
fix is per-part (carry them on `BurstPart` next to `partIndex`), not a single
group-level prop.

---

## Swept, nothing filed

So round 25 does not re-walk this ground.

1. **All four round-23 fixes re-derived from the code — they hold.**
   - `789671e9` (F-1/F-4): `BurstPart` now carries `partIndex`
     (`Chat.tsx:145,201,232`), `ToolRun` threads it (`:263`),
     `ToolActivityGroup` reads it instead of `idx` (`:129,132`),
     `ActionRow.ActionItem.partIndex` is required (`:61`), and
     `AssistantContent.tsx:104` passes `partIndex: it.idx` (true index via
     `blocks.ts:38`). Both callers now converge on one contract. The
     server-side delete allow-list (`handlers_messages.go:339-345`) is real and
     the copy-and-override rewrite (`:381-390`) preserves
     `ToolCall.ProviderExecuted` / `ToolResult.Data,MIMEType,Metadata`.
     `go test ./internal/server/ -run 'TestHandleDeleteMessagePart|TestHandleUpdateMessagePart'`
     green. *(F-1 above is a different layer: the index is now derived
     correctly, but from a client-side array that can itself diverge.)*
   - `c1c85d4c` (F-2): `ReasoningEffort` is on `MessageWire` (`wire.go:95`) and
     set by `toMessageWire` (`:135`) — confirmed present in the live wire JSON
     printed by this round's own Go probe. `ThinkingPart.tsx:71`'s divergent
     inline ternary is gone, replaced by `<EffortBadge>`.
   - `ddf931ee` / `8aa89dca` (F-3 + sibling): both broadcasts now call
     `AnnotateSessionExternalOwnership` before `hub.Broadcast`
     (`handlers_sessions.go:254`, `handlers_models.go:132`);
     `TestHandleRenameSession_…` / `TestHandleSetSessionModels_…` green.
2. **`handleSetSessionModels`'s missing mutation gate** (round 23's F-4 note,
   this round's suggested angle 2) — traced, agrees with round 23's assessment,
   not filed, with one detail sharpened. UI reachability is nil:
   `ChatToolbar.tsx:235-264` returns early for `foreignOwned` **before** both
   session-model writers — the two `ModelSelector`s (`:476-477`) *and* the
   "More" dropdown that hosts `ScopedModelsModal` (`:334-394`, `:485`). Effect
   on an in-flight foreign turn: the model itself is resolved once per turn
   (`coordinator_models.go:63`, `agent_turn.go:317,390`), so a mid-turn
   override cannot switch the running model; the only thing that *does* move is
   the `ReasoningEffort` stamped on subsequent assistant messages, because
   `currentSession` is re-read per step (`agent_turn.go:1390,1419`) and
   `PrepareStep` copies `currentSession.SmartModelReasoningEffort` onto each new
   message (`:1118`). That is a provenance inaccuracy on messages of a turn
   nobody could have started from this UI, reachable only by a hand-crafted WS
   frame. Genuinely low; not worth a gate on its own.
3. **Wire-field audit, continued** (this round's suggested angle 1). Round 23's
   list re-checked at the spots round 23's *own* changes touched:
   `ReasoningEffort` verified live on the wire (§1); `Partial` still the sole
   discriminator behind `isTerminallyFinished` (`textParts.ts:24`) and
   `updateMessageAndVerify` (`handlers_messages.go:70`). The new field round 23
   introduced, `partIndex`, was traced through **every** reader:
   `ActionRow.tsx:92` (burst thinking, now real), `Part.tsx:21` ←
   `AssistantContent.tsx:107` ← `blocks.ts:38` (real),
   `IntermediateAssistantMessage` ← `Chat.tsx:237` (real),
   `ThinkingPart.tsx:35` ← `Part.tsx:21` (real). All four derive it from a
   `Parts` array index — which is why F-1's array divergence matters, and is the
   only remaining way any of them can be wrong. `StandaloneThinking.tsx` also
   takes a `partIndex` but has **no call sites** anywhere in `web/` (only its
   own re-export at `Message/index.tsx:8`) — dead code, not a defect.
4. **`upsertSubAgentMessage` / `setSubAgentMessages`** (`store.ts:268-300`) —
   checked as F-1's obvious sibling and ruled out: the sub-agent path replaces
   messages verbatim with no merge, so a sub-agent transcript cannot resurrect a
   deleted part.
5. **`togglePinMessage`** — publishes `UpdatedEvent` like the edit handlers, but
   changes no text/thinking, so `mergePreserveContent` returns `incoming`
   verbatim and the pin reaches the UI. Fine.
6. **`trackMessageParts`** (`store.ts:302-321`) — its `_msgPartTracker` count
   goes stale after a part delete, which can place a zebra block-break at a
   shifted index. Cosmetic only (stripe boundaries), no addressing role. Not
   filed.
7. **`SubAgentBlock`** — round 22's `toolResultArrived` fix and round 21's
   `isRunning`/`completedCallIDs` reasoning re-read against the current file;
   both still correct, and `rawAgentParts` (`ToolActivityGroup.tsx:127,134,139`)
   deliberately carries no `partIndex` because sub-agent blocks expose no
   edit/delete affordance.
8. **`sitter.ts`** — full read. Tick rules (foreign-owned skip, no-todos
   retire, busy skip, nudge) are coherent; `handleSitterCommand` parsing and the
   localStorage restore path are sound. Nothing filed.
9. **`effort.ts` / `ModelSelector.tsx`** — the clamp `useEffect`
   (`ModelSelector.tsx:172-180`) only runs when `showEffortPicker` is true, so
   moving a session to a model with **no** effort knob (gemini/qwen/codex) leaves
   the stale `SmartModelReasoningEffort` in the session row rather than clearing
   it, which is the case `effort.ts:83-89`'s `clampEffort` doc says callers
   "must treat as clear the stored value". Not filed: the backend already drops
   an effort a CLI cannot accept (`internal/agent/cliprovider/effort.go`) and
   `effectiveReasoningEffort` (`coordinator_providers.go:65-75`) gates on
   `CatwalkCfg.ReasoningLevels` membership, so the stale value never reaches a
   provider. Worth knowing it is stored, not that it is dangerous.
10. **Go backend** — not re-opened per the round-19 closure. F-1's server half is
    a pre-existing allow-list interaction, not an admission/drain/checkpoint/
    kill/orphan-sweep question, and F-2/F-3 are pure frontend.

## Reproduction artefacts

All outside the repository, under the OS temp dir.

| path | what |
|---|---|
| `D:\system_artefact\Temp\crush-r24\merge_harness.mjs` | transpiles and runs the **real** `web/src/store.ts` (`upsertMessage`/`mergePreserveContent`) with a 3-entry module shim; 6 checks, 5 reproduce F-1 |
| `…\crush-r24\zz_r24_probe_test.go` + `overlay.json` | `go test -overlay` probe over the real `handleDeleteMessagePart` + `toMessageWire`; prints the exact `message_updated` payload and proves the second delete is accepted |
| `…\crush-r24\pw.config.ts` | Playwright config, `testDir: ./spec`, `baseURL http://localhost:3112` |
| `…\crush-r24\spec\merge-guard.spec.ts` | live F-1 reproduction (4 tests, incl. the no-regression control) |
| `…\crush-r24\spec\thinkingpart-collapsed.spec.ts` | live F-2 reproduction (3 tests, incl. the expanded-card control) |
| `…\crush-r24\spec\effort-in-burst.spec.ts` | live F-3 reproduction (2 tests, incl. the tool-less control) |

To re-run: start the dev server (`cd web && PORT=3112 npx rsbuild dev`),
recreate the module link (removed after the run so nothing dangles) with
`cd /d/system_artefact/Temp/crush-r24 && cmd //c "mklink /J node_modules D:\dev\go\crush\web\node_modules"`,
then `npx playwright test --config=pw.config.ts` — 9 passed.
