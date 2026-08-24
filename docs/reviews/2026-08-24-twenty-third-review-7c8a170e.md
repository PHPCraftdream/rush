# Round 23 review — `crush` fork @ `7c8a170e`

**Verdict: NO-GO.** One P1 — the Edit/Delete buttons on a `thinking` row inside a
cross-message tool accordion address the wrong part of the wrong shape entirely
(they send the row's index *within the burst* as `partIndex`), which silently
overwrites a `tool_call`'s arguments or deletes a message's terminal `finish`;
reproduced live against the real web UI. One P2 — the per-message reasoning
effort is read by five UI call sites but has never existed on the wire, and the
e2e test that "covers" it passes only because its mock hand-writes the field.

Round 22's own fix (`7c8a170e`) was re-derived and holds — see *Swept, nothing
filed* §1. Nothing it fixed is re-flagged here.

---

## F-1 (P1) — thinking-row Edit/Delete inside a cross-message tool burst sends the burst index as `partIndex`

### Where

`web/src/components/Chat.tsx:143` — the burst carrier drops the part index:

```ts
// BurstPart carries each tool/thinking part alongside its source message's
// CreatedAt. …
type BurstPart = { part: ContentPart; createdAt: number; messageID: string };
```

`web/src/components/Chat.tsx:226-236` — `buildRenderItems` **has** the true index
in scope (`pi`) and stores it for the text branch, but not for the burst branch:

```ts
      for (let pi = 0; pi < m.Parts.length; pi++) {
        const p = m.Parts[pi];
        if (p.type === "tool_call" || p.type === "tool_result" || p.type === "thinking") {
          burstParts.push({ part: p as ContentPart, createdAt: m.CreatedAt, messageID: m.ID });   // pi dropped
        } else if (p.type === "text") {
          …
          out.push({ kind: "standalonetext", messageID: m.ID, partIndex: pi, text: t });          // pi kept
        }
      }
```

`web/src/components/Chat.tsx:256-259` — `ToolRun` then re-indexes by **burst
position**:

```ts
  const items = useMemo(
    () => parts.map((bp, idx) => ({ part: bp.part, idx, createdAt: bp.createdAt, messageID: bp.messageID })),
    [parts]
  );
```

`web/src/components/Message/ToolActivityGroup.tsx:129-132` — that burst position
is handed on as if it were a part index:

```ts
    for (const { part, idx, createdAt, messageID } of items) {
      if (part.type === "thinking") {
        const text = (part as { type: "thinking"; Thinking: string }).Thinking ?? "";
        actions.push({ kind: "thinking", text, idx, key: `think-${idx}`, createdAt, messageID, partIndex: idx });
```

`web/src/components/Message/ActionRow.tsx:91-92, 111, 142, 156` — the row reads it
back and wires it straight to the two mutating WS commands:

```ts
    const messageID = item.messageID ?? "";
    const partIndex = item.partIndex ?? -1;
    …
          {messageID && partIndex >= 0 && (            // Copy / Edit / Delete strip
    …
                  onSave={(t) => { updateMessagePart(messageID, partIndex, t); setEditingThinking(false); }}
    …
            onConfirm={() => { deleteMessagePart(messageID, partIndex); setConfirmDeleteThinking(false); }}
```

`web/src/store.ts:536-542` sends them verbatim; `internal/server/handlers_messages.go`
indexes the message's own `Parts` slice with them:

- `handleDeleteMessagePart` (`:321-341`) — bounds check **only, no part-type
  guard**: `m.Parts = append(m.Parts[:p.PartIndex], m.Parts[p.PartIndex+1:]...)`
- `handleUpdateMessagePart` (`:343-393`) — bounds check plus a type switch; the
  `ToolCall` branch (`:371-377`) rewrites `Input: p.Content` and replies `ok`.

The **contrast** that makes this easy to miss: the *other* caller of the same
component, `AssistantContent.tsx:104`, feeds it `blocks.ts:38`'s items, where
`idx` genuinely is the index into `message.Parts` (`cur.items.push({ part: parts[i], idx: i })`).
The identical component is therefore correct on one path and wrong on the other.

### Why it is wrong in practice, not just in theory

`internal/agent/agent_turn.go` writes one **new** assistant message per turn step
(`:1113`, in `PrepareStep`) and puts every tool result on its **own** `role=tool`
message (`:1287`, `OnToolResult`). A message accumulates at most one
`ReasoningContent` part, created on the first reasoning delta
(`internal/message/content.go:317`), so a step's thinking part is at
`Parts[0]` — always. Meanwhile `buildRenderItems` merges N steps *and* their
interleaved `role=tool` messages into one burst, so the second and every later
thinking row in a burst gets an index that is off by however many burst entries
preceded it. In a burst of N steps, N−1 thinking rows are mis-addressed.

### Concrete failure scenarios (all reproduced)

| # | transcript shape | row sends | actually points at | operator sees |
|---|---|---|---|---|
| A | step 1 without reasoning, step 2 with | `{a2, partIndex: 2}` | `a2.Parts[2]` = the terminal `finish` | Delete succeeds. The message loses its non-`Partial` Finish → `isTerminallyFinished` goes false → server refuses every later edit ("message is still streaming and cannot be edited yet"), and `Message.tsx:77-80` hides Edit/Trash/selection. |
| B | both steps with reasoning | `{b2, partIndex: 3}` | out of range (`len(Parts)==3`) | Red toast "part index out of range" (`useWS.ts:302` surfaces any `EventError` for 8s). The affordance simply never works. |
| C | step 2 narrates between actions (`thinking`, `text`, `tool_call`, `finish`) | `{b2, partIndex: 2}` | `b2.Parts[2]` = the `edit` **tool_call** | **Silent corruption.** `handleUpdateMessagePart`'s ToolCall branch sets `Input` to the edited reasoning prose and replies `ok`. The persisted transcript now has a tool call whose arguments are English text; that message is replayed to the provider on every subsequent turn. Deleting instead removes the `tool_call` while its `tool_result` survives on the next `role=tool` message — an orphan tool result in the provider history. |

Scenario C's shape is not exotic: `Chat.tsx:220-224`'s own comment describes it
as the normal case ("A text part is the model narrating between actions ('OK,
the file exists, now let me edit it')").

### Reproduction

**(a) Live, against the real UI.** Playwright spec + config in the OS temp dir
(`D:\system_artefact\Temp\crush-r23\pw.config.ts`,
`…\spec\thinking-partindex.spec.ts`), importing the repo's own
`web/tests/helpers/mock-ws.ts` and `fixtures.ts`, driving the real rsbuild dev
server. Nothing in the repo was touched. Both tests assert the defect and pass:

```
[case A] delete_message_part -> [{"type":"delete_message_part","payload":{"messageID":"a2","partIndex":2}}]
[case B] update_message_part -> [{"type":"update_message_part","payload":{"messageID":"b2","partIndex":2,"content":"EDITED REASONING TEXT"}}]
  2 passed (15.1s)
```

(An earlier run of the same spec asserting the *correct* index failed with
`Expected: 0 / Received: 2`, i.e. the defect is what is observed, not an artefact
of how the assertion is phrased.)

**(b) Unit-level, over the real functions.** `…\crush-r23\partindex_harness.mjs`
slices `buildRenderItems`, `ToolRun`'s items mapping, `ToolActivityGroup`'s
actions builder and `blocks.ts`'s `groupPartsIntoBlocks` **verbatim out of the
files on disk**, transpiles them with the repo's own TypeScript (type erasure
only) and runs them:

```
=== A: 2-step burst, step 1 without thinking ===
  thinking row "now edit it" on message a2
    real index of that thinking part : 0
    partIndex the row actually sends : 2   <-- WRONG
    delete_message_part would        : DELETE the 'finish' part
=== B: 2-step burst, both steps with thinking ===
  thinking row "first look"  … real=0 sent=0  ok
  thinking row "second look" … real=0 sent=3  <-- WRONG   (out of range)
=== C: burst flushed by an intermediate text part ===
  thinking row "the file exists" … real=0 sent=2  <-- WRONG
    delete_message_part would        : DELETE the 'tool_call' part
    update_message_part would        : OVERWRITE the 'tool_call' part
=== D: contrast — AssistantContent path (blocks.ts idx) ===
  thinking row "reasoning": real=0 sent=0 ok

checked 5 thinking rows, 3 carried a wrong partIndex
```

### Fix direction (not applied — read-only review)

Thread the real index rather than re-deriving one. `BurstPart` (`Chat.tsx:143`)
needs a `partIndex: number`; `buildRenderItems` already has `pi` in scope at
`:227`; `ToolRun`'s mapping should pass it through alongside `idx` (keep `idx`
for React keys only); `ToolActivityGroup:132` should read that field instead of
`idx`. The `AssistantContent` caller supplies the true index already, so it can
pass `partIndex: it.idx` and both paths converge on one explicit contract. A
server-side part-type guard on `delete_message_part` (matching the one
`update_message_part` already has) would additionally stop a wrong index from
being able to remove a `finish`/`tool_call` at all — worth adding regardless,
since a bounds check alone lets any future index bug delete structural parts.

---

## F-2 (P2) — `Message.ReasoningEffort` is read by five UI sites and has never been on the wire; the e2e test that covers it fabricates the field

### Where

Written server-side on every assistant message
(`internal/agent/agent_turn.go:1118`, `ReasoningEffort: currentSession.SmartModelReasoningEffort`),
persisted (`internal/message/message.go:304`), declared client-side
(`web/src/types.ts:109`) — but **absent from `MessageWire`**
(`internal/server/wire.go:88-103`) and therefore never set by `toMessageWire`
(`:122-143`), which is the *only* conversion on every path that reaches a
browser: `events.go:64` (`message_updated`), `events.go:81` (`message_created`),
`handlers_messages.go:110` (`messages_list`). `git log -S ReasoningEffort --
internal/server/wire.go` returns nothing — it was never there.

Readers, all of which therefore evaluate `undefined`:

- `web/src/components/Message/AssistantHoverActions.tsx:59` — badge next to the model name
- `web/src/components/Message/SummaryMessage.tsx:38`
- `web/src/components/Message/AssistantContent.tsx:104,107` → `ToolActivityGroup` / `Part`
  → `ActionRow.tsx:106` (thinking-row header) and `ThinkingPart.tsx:48,71`
- `web/src/components/Message/EffortBadge.tsx:14` — `if (!effort) return null;`

`EffortBadge`'s own doc comment states the intent: *"Shown unconditionally (no
provider gate) so GLM/zai messages get their tier too — operators routinely run
GLM at high vs max and want to tell them apart at a glance."* That capability
does not exist in any running build.

### Why it survived

`web/tests/reasoning-effort.spec.ts:205-261` ("effort badge is displayed in
assistant message") injects the field by hand into the mock payload:

```ts
      payload: makeMessage({
        …
        ReasoningEffort: "max",
      }),
```

so the badge appears in the test and nowhere else. Same failure mode round 22
found in `thinking-parts.spec.ts`: a mock encoding a belief the wire does not
honour.

### Reproduction

**(a) Go, over the real conversion.** A probe injected into `package server` via
`go test -overlay` (no file written into the repo;
`D:\system_artefact\Temp\crush-r23\zz_effort_probe_test.go` + `overlay.json`):

```
$ go test -C D:/dev/go/crush -overlay=…/overlay.json ./internal/server/ -run TestZZEffortProbe -v
=== RUN   TestZZEffortProbe
    message.Message.ReasoningEffort in  = "max"
    wire JSON the browser receives     = {"ID":"m1","Role":"assistant","SessionID":"s1","Parts":[…],
      "Model":"claude-opus-5","Provider":"anthropic","CreatedAt":0,"UpdatedAt":0,
      "IsSummaryMessage":false,"Pinned":false,"Hidden":false,"AutoResumed":false,
      "BackgroundJobNotice":false}
    RESULT: field ABSENT from the wire — message.ReasoningEffort is undefined in every browser tab
--- PASS: TestZZEffortProbe (0.00s)
```

**(b) UI, both directions.** `…\crush-r23\spec\effort-badge.spec.ts` replays that
exact JSON (real server output) and then the same object with the extra
`ReasoningEffort` key (the repo's fabricated payload):

```
  ✓ real server payload -> effort badge never renders
  ✓ repo's fabricated payload (extra ReasoningEffort key) -> badge renders
  2 passed (14.6s)
```

### Notes for whoever fixes it

Adding `ReasoningEffort string \`json:"ReasoningEffort,omitempty"\`` to
`MessageWire` and one line to `toMessageWire` closes it. Two adjacent things
should be fixed in the same pass, because they only become visible once the field
is live:

- `ThinkingPart.tsx:71` duplicates `EffortBadge` inline with its own
  ternary that maps **`"max"` to `"X"`**, not `"XX"` — it would disagree with
  `EffortBadge` on the "Thoughts" header (`:48`) of the very same card.
- `web/tests/reasoning-effort.spec.ts` should stop hand-writing the field, or the
  test keeps proving nothing about the wire.

---

## F-3 (Minor) — `rename_session` broadcasts an un-annotated Session, transiently clearing `OwnedExternal` in every tab

`internal/server/handlers_sessions.go:246-251`:

```go
	sess, err := a.Sessions.Get(ctx, p.SessionID)
	…
	c.hub.Broadcast(EventSessionUpdated, sess)
```

Every other `Session` that reaches a client goes through
`AnnotateSessionExternalOwnership` first (`events.go:38,42`) or
`annotateExternalOwnership` (`handlers_sessions.go:219`). `session.Service.Rename`
(`internal/session/session_update.go:284-289`) publishes no pubsub event, so this
direct broadcast is the *only* one for a rename — and it carries
`OwnedExternal: false` (the field is `json:",omitempty"` on a zero value).
`useWS.ts:113-116` → `upsertSession` replaces the annotated row, so a session
another live process holds drops out of read-only follow mode (`ChatInput.tsx:212`,
`ChatToolbar.tsx:221`, `sitter.ts:72`) until the next `sessions_list` poll
re-annotates it — ≤5s while the tab is visible, indefinitely while it is hidden
(`useWS.ts:349-352` tears the interval down on `visibilitychange`). The Sidebar
offers Rename on foreign-owned sessions unconditionally
(`web/src/components/Sidebar.tsx:217-224`). Downstream the session lock still
guards actual writes, so this is a UI-state glitch rather than a corruption
path — hence Minor. One `AnnotateSessionExternalOwnership(a, &sess)` before the
broadcast fixes it; `handleCreateSession:63` / `handleForkSession:90` have the
same shape but are harmless (a session born in this process cannot be
foreign-locked).

---

## F-4 (Minor) — `handleUpdateMessagePart` silently drops `ToolResult.Data` / `MIMEType` / `Metadata` and `ToolCall.ProviderExecuted`

`internal/server/handlers_messages.go:371-384` rebuilds the part from scratch
instead of copying-and-overriding:

```go
	case message.ToolCall:
		m.Parts[p.PartIndex] = message.ToolCall{ID: part.ID, Name: part.Name, Input: p.Content, Finished: part.Finished}   // ProviderExecuted lost
	case message.ToolResult:
		m.Parts[p.PartIndex] = message.ToolResult{ToolCallID: part.ToolCallID, Name: part.Name, Content: p.Content, IsError: part.IsError}   // Data, MIMEType, Metadata lost
```

`Metadata` is what `ToolResultBlock.tsx:9-18` parses to render the inline diff for
`write`/`edit`/`multiedit`, so an edited tool result would silently lose its diff.
Today the UI only ever edits `text` and `thinking` parts, so neither branch is
reachable on purpose — but F-1 reaches the `ToolCall` branch *by accident*, and
the drop compounds the corruption there. Worth fixing alongside F-1.

*Also noted, no action needed:* `web/src/types.ts:23` declares `Session.CWD`,
which `internal/session/session.go`'s `Session` does not have. Nothing in
`web/src` reads it — a dead type declaration, not a bug.

---

## Swept, nothing filed

So the next round does not re-walk this ground:

1. **Round 22's fix (`7c8a170e`) re-derived, holds.** `SubAgentBlock`'s
   `toolResultArrived` (`SubAgentBlock.tsx:131-139`) scans `$messages` for a
   `tool_result` whose `ToolCallID` matches — confirmed correct: `OnToolResult`
   (`agent_turn.go:1271-1294`) only fires after `Run()` returns and creates a
   `role=tool` message **in the parent session**, which reaches `$messages`
   through `useWS.ts:169-181`. `convertToToolResult` (`agent_prompt.go:296-301`)
   does set `Name`/`ToolCallID`, so the `agent` result is routed to
   `rawAgentParts` (`ToolActivityGroup.tsx:139`) and not double-rendered. The
   `parentDone` hard-kill fallback and `completedCallIDs` (which scans **all**
   sub-session messages, including `role=tool`, so it sees the results even
   though only `role=assistant` messages are rendered) are both sound. Nothing
   re-flagged.
2. **Full wire-field audit** as round 22 recommended — every field on `PartWire`
   / `MessageWire` / `session.Session` traced from its Go writer(s) to every
   `web/src` reader:
   - `Finished` — no live readers left anywhere in `web/src` (only three
     explanatory comments). Still sent; harmless.
   - `Partial` — read by `textParts.ts:24` and `SubAgentBlock.tsx:156,162`;
     matches `Message.IsFinished()` (`internal/message/content.go:270-277`)
     exactly. `AddFinish` (`:501-510`) removes any prior Finish, so a message
     never holds both a partial and a terminal one. ✓
   - `Reason` / `Message` / `Details` — the two cross-language string contracts
     both still match their Go constants: `"Stream stalled"` vs
     `streamStalledFinishTitle` (`coordinator_run.go:54`) and
     `AWAITING_ANSWER_TITLE` vs `awaitingAnswerStoppedFinishText`
     (`question_stop.go`), including the `"QUESTION: "` / `"Suggested options: "`
     / `"This is not a crash"` markers `parseAwaitingAnswer` slices on. ✓
   - `Hidden`, `IsSummaryMessage`, `Pinned`, `AutoResumed`,
     `BackgroundJobNotice`, `IsError`, `Metadata`, `Usage` (incl. the
     `CacheHitRatio` null / `Estimated` conventions) — all present on the wire
     and read consistently. ✓
   - `OwnedExternal` / `OwnedByPID` — annotated on every broadcast path except
     the rename handler (→ F-3). ✓ otherwise
   - `ReasoningEffort` — the one field with readers and no writer (→ F-2).
   - Exhaustive cross-check: every PascalCase property read off a message/session
     in `web/src` was enumerated and matched against the Go structs;
     `ReasoningEffort` (message-level) and `CWD` are the only two with no Go
     counterpart.
3. **`updateMessageThinking`'s "first ReasoningContent wins"**
   (`handlers_messages.go:296-310`) — checked as a possible sibling of F-1 and
   ruled out: `AppendReasoningContent` (`content.go:317-336`) only ever creates a
   second thinking part if none exists, so an agent-written message has at most
   one. Not a wrong-target path.
4. **`IntermediateAssistantMessage`** (`Chat.tsx:234` → `partIndex: pi`) and
   **`ThinkingPart`** via `Part.tsx`/`blocks.ts` — both carry true part indices.
   They are the two other `update_message_part`/`delete_message_part` callers and
   are correct; F-1 is the only mis-indexed one.
5. **`ws.ts`** — one-shot handler detach, superseded-socket guard on `onclose`,
   reconnect backoff: all sound (round 20's fix intact).
6. **Go backend** — not re-opened per the round-19 closure. Nothing here is a
   re-litigation of admission/drain/checkpoint/kill/orphan-sweep; F-3 and F-4 are
   both WS-handler-layer field-plumbing, and F-1's server half is a missing
   part-type guard, not a lifecycle question.

## Reproduction artefacts

All outside the repository, in the OS temp dir; the working tree was not
modified (`git status --porcelain` unchanged apart from pre-existing untracked
docs):

| path | what |
|---|---|
| `D:\system_artefact\Temp\crush-r23\partindex_harness.mjs` | slices the real `buildRenderItems` / `ToolRun` mapping / `ToolActivityGroup` actions builder / `blocks.ts` out of the sources, transpiles with the repo's TypeScript, runs them |
| `…\crush-r23\zz_effort_probe_test.go` + `overlay.json` | `go test -overlay` probe over the real `toMessageWire` |
| `…\crush-r23\pw.config.ts` | Playwright config pointing at a temp `testDir`, reusing `web/`'s dev server on port 3111 |
| `…\crush-r23\spec\thinking-partindex.spec.ts` | live F-1 reproduction (2 tests) |
| `…\crush-r23\spec\effort-badge.spec.ts` | live F-2 reproduction (2 tests) |

To re-run the Playwright specs, recreate the module link first (it was removed
after the run so nothing dangles):
`cd /d/system_artefact/Temp/crush-r23 && cmd //c "mklink /J node_modules D:\dev\go\crush\web\node_modules"`.
