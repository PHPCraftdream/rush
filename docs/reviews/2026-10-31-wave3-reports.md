# Review wave 3 — 2026-10-31

Thirteen read-only reviewers (`rush run --role reviewer`, rush-next 0.2.0-alpha.19) read the repository at
`origin/main` `2eb4c371` (after #312 credential-isolation containment and #313 shard ceiling). Nothing was
compiled or run by them. Scopes: credential isolation, CI system, reasoning pipeline, deploy/release,
test-suite health, coordinator concurrency, MCP invariants, tool-layer security, providers/network,
persistence, server/web, config/CLI/sessions, architecture.

Rules (CLAUDE.md "Review Stop Rule"): P0/P1 need a concrete reproducible scenario and are fixed in this
cycle; P2/P3 go to the backlog and are not fixed in this cycle. Each report below is the reviewer's text
verbatim, followed by the orchestrator's own verification against the code (zero trust: a reviewer's claim
is a claim until the code path has been read). Task numbers refer to the session task list.

Verification labels used by the orchestrator: CONFIRMED (code path read by the orchestrator),
PARTLY CONFIRMED (mechanism read, consequence or trigger frequency not shown), NOT CONFIRMED.

## Index

| Report | Scope | Reviewer's top severity | After orchestrator verification | Tasks |
|---|---|---|---|---|
| W3-01 | credential isolation | 4 x P0 | 4 confirmed (3 fixable now; the restart gap needs the operator's decision) | #1682, #1683, #1684 |
| W3-02 | CI system | 2 x P1 | 0 P1 (both downgraded to P2, no branch protection on `main`; tiering gap is by design) | backlog #1700 |
| W3-03 | reasoning pipeline | 3 x P1 | 2 confirmed (compaction), 1 partly confirmed (empty `reasoning_content`) | #1678, #1679, #1680 |
| W3-04 | deploy / release / npm | 3 x P1 | 1 confirmed (Unix npm layout), 2 downgraded to P2 | #1681 |
| W3-05 | test-suite health | 3 x P1 | 2 confirmed harness defects, 1 not re-verified | #1699 |
| W3-06 | coordinator concurrency | 3 x P1 | 1 partly confirmed, 2 not re-verified (reproduction first) | #1687, #1688, #1689 |
| W3-07 | MCP invariants | 1 x P0, 3 x P1 | 1 confirmed (rated P1), 3 not re-verified (reproduction first) | #1701, #1702, #1703 |
| W3-08 | tool layer security | 3 x P0 | 1 P0 confirmed (grep argv injection), 1 P1 confirmed (git guard), 1 downgraded to P2 | #1685, #1686 |
| W3-09 | providers / network | 2 x P1 | 1 confirmed (peak hours), 1 inferred (stall retry) | #1692, #1693 |
| W3-10 | persistence | 2 x P1 | 2 confirmed | #1690, #1691 |
| W3-11 | server / web UI | 3 x P1 | 1 confirmed (oversized attachment), 1 partly, 1 not re-verified | #1694, #1695, #1696 |
| W3-12 | config / CLI / sessions | 2 x P0, 1 x P1 | 2 confirmed (rated P1), 1 not re-verified | #1697, #1698 |
| W3-13 | architecture | none | none; 7 structural P2 recommendations, 1 doc contradiction confirmed | backlog #1700 |

Also carried over: the ten unprocessed P2/P3 items of the 2026-10-29 round (`backlog.md`, ignored directory) are
tracked as task #1677.

### How the tasks are meant to be worked

1. Tenant isolation first (#1682, #1683), then the 2-token ripgrep argv fix (#1685). #1684 (persistent
   "credentials required" marker) needs the operator's go because it touches session persistence.
2. One work package for "what history looks like after a compaction": #1678, #1679, #1691 (same files, same
   invariant; reproduction tests first).
3. Small independent fixes: #1692, #1681, #1690, #1697, #1694, #1701, #1686, #1699.
4. Items marked `P1?` (#1680, #1687, #1688, #1689, #1693, #1695, #1696, #1698, #1702, #1703) start with a
   deterministic reproduction; one that cannot be reproduced drops to P2 (Review Stop Rule), and in the MCP
   subsystem and the mailbox/queue code no new guards are added.
5. Agents never run `go test`; every test run is serial under `capm 3g`, with revert checks.

---

## W3-03 — Reasoning/thinking pipeline

Reviewer session `rv3-03-reasoning-pipeline`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
VERIFIED by static inspection: three P1 defects remain—silent compaction excludes unsummarized recent history, compaction retries concatenate abandoned output, and persisted replay drops explicitly empty OpenAI-compatible reasoning. No P0 found. Files changed: none. Tests, builds, linters, Go commands, and Rush binaries run: none, as requested. Dependency inspection used fantasy v0.45.5, matching `go.mod:9`.

#### P0/P1 findings

##### [P1] Silent compaction hides the recent half that it never summarized
- Where: `internal/agent/agent_compaction.go:565`, `internal/agent/agent_compaction.go:588`, `internal/agent/agent_compaction.go:711`, `internal/agent/agent_prompt.go:344`, `internal/db/sql/messages.sql:18`.
- Scenario: A session contains eight unpinned messages. Put an important correction or tool result in messages 5–8. When sliding-window pressure triggers silent compaction, messages 1–4 are summarized. Send another request afterward: messages 5–8, including their reasoning and tool results, are absent from the rebuilt context despite remaining in the database.
- Mechanism: VERIFIED: silent compaction sends only `msgs[:mid]` to the summarizer, but creates its summary **after all existing messages**, then sets `SummaryMessageID` to that newly appended row. History is ordered by creation time and rowid. `getSessionMessages` subsequently takes `msgs[summaryMsgIndex:]`, retaining earlier rows only when pinned. Thus the unsummarized recent half is excluded too. The normal trigger is reachable through `internal/agent/agent_turn_step.go:143–158` and `internal/agent/agent_turn.go:888–891`.
- Fix idea: Separate the summarized-prefix boundary from the summary row’s chronological position. Rebuild history as pinned context, summary, and the retained recent suffix rather than taking a suffix beginning at the newly appended summary.
- Confidence: high; VERIFIED by code path, not runtime execution.

##### [P1] Compaction retries commit abandoned reasoning and summary text
- Where: `internal/agent/agent_compaction.go:399–436`, `internal/agent/agent_compaction.go:601–637`; `fantasy@v0.45.5/agent.go:999–1028`; `fantasy@v0.45.5/retry.go:144–162`.
- Scenario: During manual or silent compaction, the first provider attempt streams reasoning and summary text, then its connection closes before the terminal event. The retry succeeds with a complete replacement summary. For example, the failed attempt emits `Status: unfinished`; the successful retry emits `Status: complete`. The saved summary contains both attempts concatenated.
- Mechanism: VERIFIED: fantasy retries stream creation **and processing**, invoking the same callbacks again. Both compaction implementations retain one `summaryMessage`, append every reasoning/text delta to it, and register no `OnRetry` reset. A successful retry therefore commits the abandoned prefix plus the replacement output; manual compaction then deletes the old messages at `internal/agent/agent_compaction.go:529–532`. The ordinary turn stream has an explicit retry reset at `internal/agent/agent_turn_stream.go:489–519`, but compaction does not use it. Anthropic’s missing-terminal detection supplies a concrete retryable failure path at `fantasy@v0.45.5/providers/anthropic/anthropic.go:1701–1712`.
- Fix idea: Give both summary streams attempt-local accumulation or a retry hook that clears abandoned streamed content before accepting replacement output. Commit only the successful attempt’s summary.
- Confidence: high; VERIFIED by code path, not runtime execution.

##### [P1] Restarted history loses explicitly empty OpenAI-compatible reasoning
- Where: `internal/message/content.go:668`; `internal/agent/agent_turn_stream.go:356–415`; `fantasy@v0.45.5/providers/openaicompat/language_model_hooks.go:142–179`, `:432–442`, `:481–489`.
- Scenario: Use the Kimi-shaped response already represented in `fantasy@v0.45.5/providers/openaicompat/replay_test.go:388–406`: the assistant explicitly emits `reasoning_content: ""`, followed by a tool call. Let Rush persist the message, then resume or fork the session and send another request. The historical assistant tool-call message now omits `reasoning_content` instead of replaying the original empty field.
- Mechanism: VERIFIED: the compatible-provider mapper deliberately creates an empty reasoning block and distinguishes its presence from absence. Rush captures and persists that block, but `ToAIMessage` drops it because its text and provider metadata are empty. On replay, the compatible mapper sets `reasoning_content` only when it receives a reasoning part. The live fantasy step preserves the distinction; persisted replay does not. The mapper documents the field’s requirement for thinking-enabled Kimi tool-call messages at `fantasy@v0.45.5/providers/openaicompat/language_model_hooks.go:481–485`; actual remote rejection was not exercised.
- Fix idea: Preserve explicitly received empty compatible reasoning separately from an absent block, and retain that presence during replay. Avoid a blanket change that creates empty reasoning/text blocks for unrelated providers.
- Confidence: high for deterministic field loss; VERIFIED by code. Provider rejection is INFERRED from the mapper’s documented contract.

#### Backlog (P2/P3)
- [P2] `web/src/components/Message/ThinkingPart.tsx:40` — saving a later thinking card sends no part index; `internal/server/handlers_messages.go:315–328` always replaces the first reasoning block, so this renderer edits the wrong block (VERIFIED).
- [P2] `internal/agent/agent_turn_stream.go:376–387` — reasoning deltas do not start checkpointing; the ticker starts only on text at `:423`, leaving reasoning-only progress unpersisted between block start and end if the process dies (VERIFIED).
- [P2] `web/src/store.ts:301–314` — a nonterminal regression rebuild groups all thinking before all text/tools, losing interleaved display order and retaining abandoned content during a shorter retry until a terminal update arrives (VERIFIED).
- [P2] `fantasy@v0.45.5/providers/openai/responses_language_model.go:1166–1174` — reasoning completion emits the metadata captured at item-added without incorporating `done.Item.EncryptedContent`; an encrypted payload supplied only at completion is lost (VERIFIED).
- [P2] `fantasy@v0.45.5/providers/openrouter/language_model_hooks.go:235`, `:334` — streaming examines only the first `reasoning_details` element per chunk; coalesced text/signature or multiple-block details beyond that element are ignored; Vercel has the same first-element handling at `providers/vercel/language_model_hooks.go:273`, `:382` (VERIFIED).

#### Checked and found sound
- VERIFIED: sequential reasoning blocks are captured separately and replayed in stored part order; signed, redacted, Google-signature, and encrypted Responses metadata are attached per block (`internal/agent/agent_turn_stream.go:356–415`; `internal/message/content.go:664–695`).
- VERIFIED: persistence preserves reasoning fields and accepts both flat and fantasy-wrapped Responses metadata (`internal/message/parts_codec.go:82–97`; `internal/message/responses_metadata_decode.go:12–30`). Existing round-trip tests were read, not run.
- VERIFIED: session forking copies raw parts JSON within one transaction, avoiding a reasoning decode/re-encode loss (`internal/session/session_fork.go:170–215`).
- VERIFIED: the ordinary turn’s retry path clears abandoned text/reasoning lazily, and a new assistant step clears its retry flag (`internal/agent/agent_turn_stream.go:349–352`, `:489–519`).
- VERIFIED: summary replay converts the summary to user role, whose encoder includes text/attachments rather than reasoning parts; continuation prompts quote only visible text (`internal/agent/agent_prompt.go:352–353`; `internal/message/content.go:629–661`; `internal/agent/coordinator_retry_classify.go:30–40`).
- VERIFIED: web display receives thinking text, not replay signatures or encrypted reasoning payloads (`internal/server/wire.go:165–170`).

#### FILES READ

(63 entries not reproduced here)

### Orchestrator verification

- P1-1 (silent compaction hides the recent half): **CONFIRMED** by the orchestrator. `runSummarizeSilent` summarises only `msgs[:mid]` (agent_compaction.go:566-567), then creates the summary row with `messages.Create` (:588), i.e. AFTER every existing row; history is ordered `created_at ASC, rowid ASC` (messages.sql:18); `SetSummaryAndUsage` points `SummaryMessageID` at that newest row (:711); `getSessionMessages` then returns `msgs[summaryMsgIndex:]` plus pinned rows only (agent_prompt.go:344-358). The unsummarised, non-pinned recent half therefore stays in the database but never reaches the next prompt. It runs after the turn that crossed the sliding-window threshold (agent_turn_step.go:143-158, agent_turn.go:888). No test was found that asserts the retained half. Trigger: context nearly full (normal for long sessions). Needs a reproduction test first. Task created.
- P1-2 (compaction retries concatenate abandoned output): **CONFIRMED**. fantasy v0.45.5 wraps stream creation AND processing in one retry (agent.go:1001-1027) and exposes `OnRetry` (retry.go:144-148); neither summary stream (agent_compaction.go:601-637 and the manual path) registers `OnRetry`, so every delta of the failed attempt stays in the one `summaryMessage`. The ordinary turn resets on retry (agent_turn_stream.go:489-519). Task created.
- P1-3 (explicitly empty openai-compatible reasoning dropped on replay): **PARTLY CONFIRMED**. The field loss is deterministic: `ToAIMessage` drops a reasoning block with empty text and empty metadata (message/content.go:668, the filter extended in #308), and every new turn rebuilds history from stored parts, not only after a restart. That a provider then rejects the request is INFERRED from the mapper's own comment about Kimi thinking tool-call messages (openaicompat/language_model_hooks.go:481-485); no real rejection was exercised. Kept at P1 because a thinking-enabled Kimi session hits it on the next turn after the first tool call. Task created; first step is to confirm with the provider's documented requirement or a captured 400.
- Backlog P2 items (web ThinkingPart edits the first block only; reasoning-only progress not checkpointed; web store regroups thinking before text on rebuild; fantasy Responses reasoning-done ignores `done.Item.EncryptedContent`; fantasy OpenRouter/Vercel streaming reads only the first `reasoning_details` element): recorded here, not fixed in this cycle. The last two are dependency (fantasy) defects.

---

## W3-04 — Deploy, build, release, npm, update notice

Reviewer session `rv3-04-deploy-release`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
The deployment and npm launch paths have useful safeguards, but I found three P1 failure scenarios: Unix npm deployment replaces the launcher rather than the packaged binary, concurrent deployment can poison the content-addressed binary cache, and an unlocked cleanup sweep can remove another deploy’s rollback source. No P0 found. Findings are based on code inspection, not runtime reproduction; no files were changed and no builds, tests, linters, Go commands, or Rush binaries were run.

#### P0/P1 findings

##### [P1] Unix npm deployment replaces the launcher symlink instead of the platform binary
- Where: `deploy.go:325`, `deploy.go:331`, `deploy.go:348`, `deploy.go:425`; `internal/deploy/deploy.go:288`, `internal/deploy/deploy.go:462`
- Scenario: Install Rush globally with npm on Linux using the standard layout: `<prefix>/bin/rush` is a symlink to the package’s `bin/rush.js`, while platform packages reside under `<prefix>/lib/node_modules`. Put `<prefix>/bin` on PATH and invoke the deployment script.
- Mechanism: Destination discovery assumes `node_modules` is directly beneath the directory containing the PATH entry, so it checks `<prefix>/bin/node_modules/...`, not the installed platform package (`deploy.go:331`; `internal/deploy/deploy.go:462`). It then classifies `<prefix>/bin/rush` as replaceable: the pathname has no script extension, and `os.Stat` follows its symlink to the executable JS launcher (`internal/deploy/deploy.go:288–301`). The final rename replaces the symlink itself with a native binary (`deploy.go:425`). The actual npm platform binary remains unchanged, and the deployment silently bypasses the wrapper’s cache and launch handling.
- Fix idea: Resolve npm launcher symlinks before classifying destinations, and discover the matching platform package from the resolved package location. Never treat an extensionless symlink to a JS launcher as a native executable merely because its target has executable permissions.
- Confidence: high; VERIFIED by inspection of the destination-selection and replacement code against the stated npm layout.

##### [P1] Hashing and copying different binary generations permanently mislabels a cache entry
- Where: `npm/rush/bin/rush.js:72`, `npm/rush/bin/rush.js:81`, `npm/rush/bin/rush.js:90`, `npm/rush/bin/rush.js:96`, `npm/rush/bin/rush.js:120`; `deploy.go:425`
- Scenario:
  1. A launcher hashes installed binary A and obtains hash H(A), with no existing H(A) cache entry.
  2. Before its `copyFileSync`, a deployment atomically replaces that installed binary with B.
  3. The launcher copies B into the staging directory and publishes it under H(A).
  4. A second installation containing genuine A, sharing the same user cache, launches Rush.
- Mechanism: Hashing opens and closes the source independently of the subsequent pathname-based copy (`rush.js:72`, `rush.js:90`, `rush.js:194–208`). Nothing checks that the copied bytes match the calculated key. Consequently B is published under H(A). Later launches of genuine A trust the existing cache entry solely because the path exists, hardlink it, and execute B (`rush.js:81`, `rush.js:120`, `rush.js:147–158`). The cache can therefore return the wrong executable even after the original deployment race has ended.
- Fix idea: Copy into private staging first, hash that completed staging file, and publish it under that hash. Select the launch executable from that same immutable staged generation.
- Confidence: high; VERIFIED code-level interleaving. Not executed.

##### [P1] Cleanup runs outside the deploy lock and can destroy an in-flight rollback source
- Where: `deploy.go:150`, `deploy.go:166`; `internal/deploy/deploy.go:163`, `internal/deploy/deploy.go:169`, `internal/deploy/deploy.go:221`
- Scenario:
  1. On Windows, deploy A holds the deploy lock and renames a running destination executable aside.
  2. The old process exits, making the aside file deletable.
  3. Deploy B reaches its cleanup sweep before attempting to acquire A’s lock and deletes that fresh aside file.
  4. A’s temporary-file rename fails—for example, because antivirus has opened the new temporary executable without delete sharing.
  5. A attempts rollback, but the aside file no longer exists.
- Mechanism: `SwapRenameAside` relies on the aside file remaining available between its first rename and rollback (`internal/deploy/deploy.go:163–169`). However, every deployment sweeps before acquiring the exclusive lock (`deploy.go:150–166`), and `.old-*` files have neither an age gate nor an ownership check (`internal/deploy/deploy.go:221–229`). B can therefore delete A’s recovery copy while A owns the supposedly exclusive replacement section. A then returns with the installed destination missing; the caller also removes its temporary new copy (`deploy.go:423`).
- Fix idea: Acquire the deploy lock before sweeping and keep it through replacement and verification. This prevents cooperating deploys from deleting another deploy’s active rollback material.
- Confidence: high; VERIFIED code-level failure scenario, including the rollback dependency. The Windows timing and injected I/O failure were not exercised.

#### Backlog (P2/P3)
- [P2] `internal/update/update.go:71–82` — Availability uses inequality, not semantic ordering: current `0.2.0-alpha.19` versus latest `0.2.0-alpha.9` reports an update; stable downgrades also qualify (VERIFIED).
- [P2] `internal/version/version.go:303–311`, `internal/update/update.go:52–62`, `internal/server/update_notice.go:62` — Actual checkout versions such as `<hash>-0.2.0-alpha.19` are not recognized as development builds, contradicting the notice-suppression contract (VERIFIED).
- [P2] `internal/update/update.go:140–145`, `internal/update/update.go:97` — Successful JSON responses need no nonempty or valid `tag_name`; `{}` becomes an empty latest version and can produce a bogus update notice (VERIFIED).
- [P2] `npm/rush/bin/rush.js:135`, `npm/rush/bin/rush.js:226–235` — Killed launchers leave `.tmp-launch-*` directories that every future sweep skips permanently; on filesystems requiring copy fallback, each orphan retains a full binary (VERIFIED).
- [P2] `.github/workflows/publish-fork-npm.yml:50`, `.github/workflows/publish-fork-npm.yml:90–106` — Manual dispatch accepts a tag but never checks it out; the dry run labels binaries with the supplied tag’s version while building the dispatch-selected ref (VERIFIED).
- [P2] `.github/workflows/publish-fork-npm.yml:268–272` — Numeric prereleases such as `0.2.0-1` fall through to the `latest` npm dist-tag despite the stated stable-only invariant (VERIFIED).
- [P2] `.github/workflows/publish-fork-npm.yml:290–295` — Publication is not retry-safe: after some platform packages publish and a later publish fails, rerunning attempts the already-published immutable versions before reaching the missing packages (INFERRED).
- [P2] `.github/workflows/publish-fork-npm.yml:188–245`, `internal/version/version.go:46–47` — The version guard checks `VersionLine`, which ignores the injected `Version`; it cannot detect the broken `Version` injection its documentation promises to catch (VERIFIED).
- [P2] `build.go:100–102`, `internal/version/version.go:74–78` — The injected eight-character commit plus `-dirty` is truncated back to eight characters in `VersionLine`, hiding the dirty provenance that the build script deliberately records (VERIFIED).
- [P2] `.claude/skills/deploy.md:5–26` — Deployment instructions still build and kill the legacy `crush` executable and perform a hard-coded copy, contradicting the current Rush script’s non-killing, destination-discovery behavior (VERIFIED).
- [P3] `.github/workflows/publish-fork-npm.yml:124`, `internal/version/version.go:91–101`, `internal/version/version.go:165–166` — Release builds do not inject `BuildID=Commit` as documented; they fall back to executable mtime, making the displayed identifier dependent on installation/cache copying (VERIFIED).
- [P3] `internal/deploy/deploy_live_test.go:82–88` — Checking `cmd.ProcessState` before `Wait` cannot establish continued process liveness; the integration test’s claimed survival assertion is ineffective (VERIFIED).

#### Checked and found sound
- VERIFIED: Ordinary second-rename failure attempts immediate restoration, and restoration failure names both recovery paths; corresponding fault-injection cases are present (`internal/deploy/deploy.go:162–176`; `internal/deploy/deploy_test.go:394–473`). The separate sweep race is reported above.
- VERIFIED: Lock contention uses nonblocking OS locks and cancellable polling, rather than an unjoinable blocking-acquisition goroutine (`internal/filelock/file_lock.go:150–175`; `internal/filelock/oslock_windows.go:15–31`; `internal/filelock/oslock_unix.go:14–15`).
- VERIFIED: PowerShell PATH writes use a single-quoted literal with embedded apostrophes doubled; they do not interpolate PATH content as PowerShell expressions (`deploy.go:250–254`).
- VERIFIED: Update checks run asynchronously, have context and HTTP deadlines, cap response reads, and return without notifying on network or decoding failure (`internal/server/server.go:139–145`; `internal/server/update_notice.go:49–55`; `internal/update/update.go:118–145`).
- VERIFIED: The Rush wrapper pins a private executable before sweeping other cache generations, with copy fallback if pinning fails; this protects an already-selected launch from ordinary cache eviction (`npm/rush/bin/rush.js:113–147`).
- VERIFIED: npm manifests pin platform dependency versions and contain no install/postinstall hooks; the launcher passes argv without a shell (`npm/rush/package.json:23–39`; `npm/crush/package.json:23–39`; `npm/platform/package.json.tmpl:1–17`; `npm/rush/bin/rush.js:158`).
- VERIFIED: Manual npm publishing is forced to dry-run, and upstream release/snapshot jobs are disabled for the fork (`.github/workflows/publish-fork-npm.yml:257–277`; `.github/workflows/release.yml:18`; `.github/workflows/snapshot.yml:19`).

#### FILES READ

(41 entries not reproduced here)

### Orchestrator verification

- P1-1 (Unix npm deploy replaces the launcher symlink): **CONFIRMED** in the code path, impact limited to the dev deploy tool on Linux/macOS. `resolveDests` looks for the platform package only at `<PATH dir>/node_modules/...` (deploy.go:331, internal/deploy/deploy.go:462), which exists beside the shim on Windows but not in the standard Unix layout (`<prefix>/bin/rush` -> package `bin/rush.js`, packages under `lib/node_modules`). Candidate (c) then accepts the PATH hit itself: `IsReplaceableExe` tests the extension of the symlink path (empty) and `os.Stat` follows the link to an executable `rush.js` (internal/deploy/deploy.go:288-301), so the symlink is replaced by the native binary and the packaged binary stays old. Kept at P1 because the tool reports success and `rush --version` is fresh, hiding the mismatch; fix is small (resolve symlinks before classifying, locate the platform package from the resolved package directory). Task created.
- P1-2 (launcher hashes one generation and copies another): **mechanism CONFIRMED, downgraded to P2.** `hashFileSync(binary)` at npm/rush/bin/rush.js:72 and `copyFileSync(binary, tmpPath)` at :90 are independent opens with no check that the copied bytes match the key. It needs a deploy replacing the installed binary inside the first-launch window and a later launch of the genuine old build from the same user cache (for example a rollback). Narrow; backlog.
- P1-3 (sweep runs outside the deploy lock): **ordering CONFIRMED, downgraded to P2.** deploy.go sweeps leftovers at :146-154 and takes the lock at :163-171; `.old-*` files have no age or ownership gate. Needs two concurrent deploys plus a failing second rename in the first (antivirus on the new temp file) before the rollback source is missing. Narrow, but the fix is one line of ordering (sweep after the lock is held), so it is bundled with P1-1 in one task.
- Backlog P2/P3 (update availability by inequality — a pre-release current with a different pre-release latest reports an update even when latest is older; dev builds named `<hash>-0.2.0-alpha.N` not recognised as development; a `{}` response gives an empty latest; orphaned `.tmp-launch-*` dirs never swept; publish-fork-npm.yml dispatch tag not checked out / numeric prerelease falls to `latest` / publish not retry-safe / version guard ignores the injected `Version`; `-dirty` truncated in `VersionLine`; stale `.claude/skills/deploy.md` — already rewritten in the pending skill PR; release builds do not inject BuildID; a live-process assertion in deploy_live_test.go is ineffective). The update-notice items are dormant while no GitHub release exists (the releases endpoint returns nothing for this repository today). Recorded, not fixed in this cycle.

---

## W3-01 — Credential isolation

Reviewer session `rv3-01-credential-isolation`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
**VERIFIED by code inspection:** tenant isolation is not complete. I found four P0 security-boundary crossings: reserved provider-name dispatch, ambient Bedrock authentication, interrupting a delegated child, and scheduled wakes after restart. The ordinary credentialed turn, reviewer exclusion, and explicit durable-enqueue refusals have useful safeguards, but those safeguards do not cover these paths. No files changed; no builds, tests, linters, Go commands, or Rush binaries were run.

#### P0/P1 findings

##### [P0] Tenant provider name `openai-codex` selects the operator’s OAuth credentials
- Where: `internal/agent/credentials.go:566`, `internal/agent/credentials.go:606`, `internal/agent/coordinator_providers.go:787`, `internal/agent/coordinator_providers.go:828`, `internal/agent/codex_token_source.go:28`, `internal/agent/codexprovider/provider_acquire.go:74`
- Scenario: Open a Client whose configuration contains a logged-in `openai-codex` provider. Call `RunWithCredentials` with a credential named `openai-codex`, an otherwise permitted type such as `ProviderTypeOpenAI`, a tenant API key and endpoint, and smart/fast choices referencing that credential.
- Mechanism: Credential validation permits this provider name. `credentialProviderConfig` copies it into `ProviderConfig.ID`; the literal-credential construction path then enters the ordinary provider dispatcher. That dispatcher checks **ID before Type**, and the `openai-codex` branch ignores the supplied key and endpoint, constructing a token source backed by `c.cfg` (`coordinator_providers.go:828–832`). `codexTokenSource.Current` reads the configured operator OAuth token (`codex_token_source.go:32–40`), which the request uses as its bearer credential (`provider_acquire.go:75–79`). Tenant prompts therefore reach the operator’s Codex account despite strict fallback being disabled.
- Fix idea: Keep literal tenant construction separate from configured-provider identity dispatch. Reject unsupported OAuth identities or dispatch exclusively by the tenant’s declared Type without accessing configured token sources.
- Confidence: high; **VERIFIED** by the complete construction-to-send code path; not runtime-tested.

##### [P0] Empty tenant Bedrock keys silently select the host’s bearer token
- Where: `internal/agent/credentials.go:210`, `internal/agent/credentials.go:580`, `internal/agent/coordinator_providers.go:787`, `internal/agent/coordinator_providers.go:711`, `sdk/sdk.go:583`
- Scenario: The embedding process has `AWS_BEARER_TOKEN_BEDROCK` set to the operator’s token. Supply a valid `CredentialSet` with a Bedrock credential whose `APIKey` is empty and smart/fast choices referencing it. Leave `AllowConfiguredRoleFallback` false.
- Mechanism: Validation checks provider names, types and model references but does not require an API key (`credentials.go:220–248`). The supposedly literal construction path reaches `buildBedrockProvider`, where an empty supplied key explicitly falls back to `os.Getenv("AWS_BEARER_TOKEN_BEDROCK")` and passes that operator token to `bedrock.WithAPIKey` (`coordinator_providers.go:719–723`). This contradicts the SDK promise that environment credentials are not merged into a credentialed call (`sdk.go:583–589`).
- Fix idea: Reject missing literal credentials before provider construction. The tenant path must never use Bedrock’s environment/default authentication fallback; retain that behavior only for configured-provider calls.
- Confidence: high; **VERIFIED** that the operator environment token is selected and handed to provider construction; no live provider request was executed.

##### [P0] `inject_agent(interrupt=true)` rebuilds a tenant child on the operator’s models
- Where: `internal/agent/tools/inject_agent.go:39`, `internal/agent/coordinator_agent_control.go:211`, `internal/agent/coordinator_agent_control.go:251`, `internal/agent/coordinator_models.go:114`, `internal/agent/coordinator_run.go:119`, `internal/agent/agent_run.go:729`
- Scenario: A credentialed root launches an `agent` delegation, which runs detached. While that tenant child is still streaming, the root calls `inject_agent` with its child ID, a replacement instruction, and `interrupt: true`. The Client has ordinary operator smart/fast providers configured.
- Mechanism: The direct-child authorization check succeeds, but the interrupt branch calls `resolveSessionModels`, not a credential-aware resolver or the child driver’s existing template (`coordinator_agent_control.go:251–255`). That resolver starts from the Client’s configured models (`coordinator_models.go:114–118`). `buildCall` pins those operator models and does not attach a `CredentialSet` (`coordinator_run.go:119–159`). The replacement is installed on the child’s actual driver (`coordinator_agent_control.go:262`); cancellation drains it into the next turn (`agent_turn_failure.go:303–306`, `agent_run.go:729–762`). That next turn sends the child’s tenant history to the operator provider. The active-call credential preservation implemented for interrupt-inject polling is bypassed.
- Fix idea: Build the replacement from the child’s published active-call snapshot, preserving its credentials and pinned models, with a generation-checked handoff. If that binding cannot be obtained, refuse rather than resolving configured models.
- Confidence: high; **VERIFIED** by the model-resolution, mailbox replacement, cancellation-drain and next-turn paths; not runtime-tested.

##### [P0] A tenant’s durable wake resumes its transcript on configured providers after restart
- Where: `internal/agent/credentials.go:337`, `internal/agent/credentials.go:354`, `internal/agent/coordinator_wake_tools.go:106`, `internal/agent/wake_scheduler.go:88`, `internal/agent/wake_scheduler.go:224`, `internal/agent/agent_drain.go:110`, `internal/agent/agent_drain.go:205`, `internal/app/app_agent_setup.go:219`
- Scenario: On a persistent Client, run a tenant call with strict per-call credentials and no custom disk-provider fence. Have it schedule `wakein` sufficiently far in the future and finish. Close the Client, then reopen the same workspace/data directory with operator providers configured, without explicitly resupplying the tenant credentials. Wait until the schedule fires.
- Mechanism: Wake creation persists the session owner and message but no credential requirement (`coordinator_wake_tools.go:118–121`). The credentialed-root marker is only a coordinator-local map (`credentials.go:354–367`). Reopening constructs a new coordinator and automatically starts its durable scheduler (`app_agent_setup.go:136–147,219–221`). The scheduler fires the persisted schedule, inserts a wake-worthy notice and calls `wakeSession` (`wake_scheduler.go:234–257`; `internal/session/wake_schedule_fire.go:154–156`). The new coordinator has no root marker, so `drainCallFor` passes its only credential guard and resolves configured models (`agent_drain.go:111–113,205`). The resulting drain loads the persisted conversation before streaming (`agent_turn.go:449,798–802`). Restart therefore converts deferred tenant work into operator-provider traffic.
- Fix idea: Persist a non-secret credential-required marker before accepting credentialed work. Every automatic continuation must fail closed when that requirement exists without an explicitly supplied live binding; do not persist the credentials themselves.
- Confidence: high; **VERIFIED** by the durable schedule, startup wiring, marker storage and drain execution paths; not runtime-tested.

#### Backlog (P2/P3)
- [P2] `internal/agent/agentic_fetch_tool.go:174` — Fetch looks up its tenant-selected fast provider in the Client configuration, so a valid tenant-only provider name causes “fast model provider not configured” before delegation (VERIFIED).
- [P2] `internal/agent/coordinator_subagents.go:187` — Credentialed `agentic_fetch` selects tenant worker/smart in `runSubAgent`, replacing the fast model explicitly selected by its caller; this breaks the fetch role’s model choice without crossing tenants (VERIFIED).
- [P2] `internal/agent/coordinator_providers.go:744` — The accepted `google-vertex` credential type reaches a builder that never receives its API key or base URL; ambient-auth consequences need confirmation against fantasy v0.45.5 (INFERRED).
- [P3] `internal/app/app_run_request.go:249` — The public aliased request’s credential documentation still says uncovered roles fall back normally, omitting the strict-default refusal and explicit fallback opt-in (VERIFIED).
- [P3] `sdk/README.md:189` — The new paragraph accurately limits the marker to one Client lifetime, but does not warn that reopening persistent data can automatically transmit the tenant transcript using configured credentials (VERIFIED).

#### Checked and found sound
- **VERIFIED:** `Client.RunWithCredentials` clones the bundle, and coordinator admission retains another deep copy rather than mutating caller-owned credential maps/slices (`sdk/sdk.go:632`, `internal/agent/credentials.go:182–200,327`).
- **VERIFIED:** Missing fast credentials fail before ordinary fallback resolution unless explicitly allowed (`internal/agent/credentials.go:456–470`).
- **VERIFIED:** PR #312 chains the caller’s admission callback, marks only after successful setup, and treats credential-binding drain refusals as terminal for timed rechecks (`internal/agent/credentials.go:337–345`, `internal/agent/coordinator_run.go:246–253`, `internal/agent/coordinator_wake.go:88–96`).
- **VERIFIED:** Active credentialed interrupt-inject polling copies the active call and uses a generation-checked in-process replacement instead of serializing credentials (`internal/agent/coordinator_interrupt.go:334–340,569–599`).
- **VERIFIED:** Orphan recovery and detached enqueue explicitly refuse calls carrying credentials before serialization (`internal/agent/agent_ownership.go:505–510`, `internal/agent/coordinator_interrupt.go:723–727`).
- **VERIFIED:** Batch agent/fetch payloads carry a boolean marker, while credential snapshots stay in a keyed, single-consumption escrow; missing escrow bindings refuse launch (`internal/agent/batch_credentials_marker.go:23–38`, `internal/agent/batch_credentials_escrow.go:89–102`, `internal/agent/batch_install_agent_driver.go:60–74`).
- **VERIFIED:** The automatic reviewer pass excludes credentialed requests (`internal/app/app_run.go:630`).
- **VERIFIED:** Inline full/silent compaction uses the turn’s pinned smart model; title generation uses captured fast/smart models rather than rereading shared models (`internal/agent/agent_turn.go:842,889`, `internal/agent/agent_title.go:261–308`).
- **VERIFIED:** The hook decorator forwards the existing context to the inner tool, preserving credential context values (`internal/agent/hooked_tool.go:90–94`).
- **VERIFIED:** SDK shutdown closes admission atomically and runs underlying shutdown once (`sdk/sdk.go:839–868,877–884`).

#### FILES READ

(0 entries not reproduced here)

### Orchestrator verification

- P0-1 (tenant provider name `openai-codex` selects the operator's OAuth credentials): **CONFIRMED** by the orchestrator. `CredentialSet.Validate` accepts any non-empty, unique provider name (credentials.go:216-231); `credentialProviderConfig` copies it into `ProviderConfig.ID` (:604-608); the literal-credential path calls `buildProviderWithValues`, whose inner dispatcher tests `providerCfg.ID == "openai-codex"` BEFORE the type switch and, when the coordinator has a config, builds a token provider on `codexTokenSource{store: c.cfg}` ignoring the supplied key and URL (coordinator_providers.go:828-833). A tenant call that names its provider `openai-codex` therefore sends the tenant transcript to the operator's logged-in Codex account. Reachable by any embedder that lets tenant data choose the provider name.
- P0-2 (empty tenant Bedrock key selects host credentials): **CONFIRMED**. `Validate` never requires an API key (credentials.go:216-231; `bedrock` and `google-vertex` are accepted types, :41-43); `buildBedrockProvider` takes `AWS_BEARER_TOKEN_BEDROCK` from the process environment when the key is empty, and otherwise lets the AWS SDK use its default credential chain (coordinator_providers.go:719-726) — both are host identity. `buildGoogleVertexProvider` (:744-759) never receives a tenant key at all, so ambient application-default credentials are the only possible auth (reviewer's P2 note, consequence INFERRED).
- P0-3 (`inject_agent(interrupt=true)` rebuilds a tenant child on the operator's models): **CONFIRMED**. The interrupt branch calls `c.resolveSessionModels(ctx, childSessionID)` and `c.buildCall(...)` directly (coordinator_agent_control.go:251-255) and installs the result with `InterruptAndReplace` (:262) — the same configured-model resolver that caused the fixed #312 leak, not the credential-aware `resolveCallModels`/the child driver's template. The next turn of that child carries the tenant history to the configured provider.
- P0-4 (a tenant's durable `wakein` resumes on configured providers after a restart): **CONFIRMED as the known restart gap, now with a concrete trigger.** The marker added in #312 is an in-memory map (credentials.go:354-367); a persisted wake schedule survives the process, the reopened coordinator has no marker, so `drainCallFor`'s refusal does not fire (agent_drain.go:110-113). This is exactly the open decision recorded after #312 ("a persistent non-secret flag would be needed"); the reviewer's scenario shows a normal tool (`wakein`) reaches it. The repository already has a durable per-session execution requirement (`SessionExecutionRequirement`, the disk-provider fence used by `guardTurnDiskFence` and `persistSessionExecutionIntent`); the natural fix is a credentials requirement on that same fence rather than a new mechanism. Needs the operator's go because it touches session persistence.
- Backlog (P2/P3): agentic_fetch looks up its tenant-selected fast provider in the Client configuration (VERIFIED by reviewer), credentialed agentic_fetch replaces the caller's fast model with worker/smart, google-vertex type never gets a key (INFERRED), aliased request type's docs omit the strict default, sdk/README.md paragraph does not warn that reopening persistent data can send the transcript on configured credentials (will be fixed together with P0-4).
- "Checked and found sound" entries were not re-checked by the orchestrator beyond what is cited above.

---

## W3-02 — CI system (workflows, selector, ci-gate, security)

Reviewer session `rv3-02-ci-workflows`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
The ordinary gate path has substantial fail-closed checks, but I found **two P1 validation gaps**: unrelated label events produce an unconditional green gate, and tier1 Windows selections silently omit the exact `internal/agent` package. Both are VERIFIED from the workflow/helper code paths, not live Actions runs. No files changed; no builds, tests, linters, Go commands, or Rush binaries were run.

#### P0/P1 findings

##### [P1] Unrelated label events produce a green gate without validating the revision
- **Where:** `.github/workflows/ci.yml:9`, `.github/workflows/ci.yml:28`, `.github/workflows/ci.yml:386-388`; `.github/scripts/ci_gate_decide.py:251-256`.
- **Scenario:** A PR revision has failed validation—for example, its `checks` job failed. Add or remove any label other than `full-ci`, such as `bug`. This starts another CI run for that revision, skips planning and worker jobs, and produces a successful `ci-gate` check without consulting the earlier failure.
- **Mechanism:** The workflow subscribes to all label changes but guards its validation jobs against unrelated labels. The gate nevertheless runs with `always()`. Its unrelated-label branch immediately returns `pass`, before loading the plan, inspecting `needs`, querying live revision identity, or fetching job receipts. The behavior is explicitly asserted by `.github/scripts/test_ci_gate_decide.py:179-183`. Consequently, green here means “label event ignored,” not “revision validated.” Whether this also permits merging depends on external required-check configuration, which was not inspected.
- **Fix idea:** Filter unrelated label events before creating a validation check, or have these runs require successful validation receipts for the same head/base instead of reporting unconditional success under the validation gate’s name.
- **Confidence:** High; **VERIFIED** by static workflow and helper control flow.

##### [P1] Tier1 Windows runs omit selected `internal/agent` tests
- **Where:** `.github/scripts/ci_plan/selector.py:119-120`, `.github/scripts/ci_plan/selector.py:217-219`; `.github/workflows/ci-tests.yml:493-496`, `.github/workflows/ci-tests.yml:544-545`, `.github/workflows/ci-tests.yml:615-617`; `.github/scripts/ci_gate_decide.py:79-87`.
- **Scenario:** Submit a PR without `full-ci` or a full directive that changes only an agent test—for example, add `internal/agent/selection_windows_test.go` containing a Windows-only failing regression test. The selector produces tier1 with the owning agent package selected. Linux/macOS do not compile that Windows-only test, while the Windows job never runs the selected package.
- **Mechanism:** Test-only changes seed the owner without reverse-dependency expansion. The native grouping code explicitly discards the exact `internal/agent` token. A later dedicated agent step exists only for macOS; Windows receives its dedicated `windows-agent` job only through the **full-mode** shards workflow. On tier1, the native loop can therefore find no Go group files and exit successfully at `.github/workflows/ci-tests.yml:517-518`. The gate requires the successful Windows native job receipt, but no receipt proving that agent tests actually ran.
- **Fix idea:** Add a tier1 Windows agent step/job when the exact agent package is selected, using the existing Windows batching/resource settings. Alternatively, promote Windows agent selections to full until that lane exists.
- **Confidence:** High; **VERIFIED** by static selection and execution paths. The reproduction was not executed.

#### Backlog (P2/P3)
- [P2] `.github/scripts/ci_plan/policy.py:94-100` — The `**/` branch discards the pattern’s leading prefix; `internal/skills/**/*` consequently matches unrelated paths such as `docs/example.txt`, assigning otherwise unknown non-Go inputs to skills/agent instead of triggering the conservative fallback (VERIFIED).
- [P2] `.github/scripts/ci_plan/golist.py:115-120` — Embed filenames are tested against a repository-relative package prefix without first joining their package-relative paths; an entry such as `dist/index.html` for package `web` is dropped. Combined with the overly broad mapping above, this can conceal missing resource ownership (VERIFIED).
- [P2] `.github/ci-shards/assign_shards.py:188-204` — Coverage validation checks workflow-step → table → assignment, not assignment-group → executable step. Removing a named test step while retaining its table/group entries can still pass the coverage check; assignment equality does not prove execution (VERIFIED).
- [P2] `.github/scripts/test_ci_plan.py:42-85`, `.github/scripts/test_ci_plan.py:108-290` — Selector fixtures do not exercise embed ownership, non-Go glob isolation, actual git collection, or cross-package test-import closure; the embed-capable fixture helper is unused by the test cases (VERIFIED).
- [P2] `.github/workflows/licenses.yml:68`, `.github/workflows/licenses.yml:101-116` — The cached license verdict fingerprints manifests/policy/checker but not source imports, although the report scans reachable packages. A source-only import of an already-listed, previously unreachable dependency can reuse an obsolete verdict (INFERRED).
- [P2] `.github/workflows/publish-fork-npm.yml:96`, `.github/workflows/publish-fork-npm.yml:268-271` — Valid numeric prereleases such as `npm-v0.2.0-1` are accepted but assigned the `latest` dist-tag, contradicting the intended rule that only stable releases reach `latest` (VERIFIED).
- [P3] `.github/scripts/report.py:35` — No such file is used; the stale “shadow mode—nothing runs” heading actually remains in `.github/scripts/ci_plan/report.py:35`, despite this selector now driving active CI (VERIFIED).

#### Checked and found sound
- **VERIFIED:** Ordinary gate execution rejects unsuccessful plan/check dependencies, output-contract drift, stale PR head/base identities, missing required leaf receipts, and non-success required results: `.github/scripts/ci_gate_decide.py:263-308`, `.github/scripts/ci_gate_decide.py:38-58`.
- **VERIFIED:** Full Windows assignment validation checks unique package coverage, shard/count consistency, exact isolated-agent membership, and equality with the full Windows inventory: `.github/scripts/ci_gate_decide.py:209-228`.
- **VERIFIED:** The current shard manifest has a separate exact agent assignment and one catch-all; overlapping matching groups are rejected rather than silently resolved: `.github/ci-shards/shards.toml:11-56`, `.github/ci-shards/assign_shards.py:104-123`.
- **VERIFIED:** Reverse traversal computes a transitive closure across all three OS graphs, and graph parsing consumes normal/test/external-test import lists: `.github/scripts/ci_plan/golist.py:108-113`, `.github/scripts/ci_plan/golist.py:186-200`. This establishes the traversal mechanism, not completeness for every external fixture.
- **VERIFIED:** Dependency/toolchain changes explicitly force full selection, independently of ordinary non-Go ownership: `.github/scripts/ci_plan/selector.py:230-242`.
- **VERIFIED:** Full-directive discovery uses complete checkout history and `BASE..HEAD` commit bodies; messages are processed as subprocess output rather than interpolated into shell source: `.github/workflows/ci.yml:50-53`, `.github/workflows/ci.yml:76-84`, `.github/scripts/ci_gate_decide.py:183-195`.
- **VERIFIED:** The scoped `pull_request_target` labeler neither checks out nor executes PR code and is skipped on the fork: `.github/workflows/labeler.yml:6-7`, `.github/workflows/labeler.yml:23-33`. Core CI third-party actions are SHA-pinned, and explicit core Go-cache saves are restricted to main pushes: `.github/workflows/ci.yml:95-111`, `.github/workflows/ci-tests.yml:206-217`.
- **VERIFIED:** Manual npm dispatch cannot switch the publish step out of dry-run mode; its tag input enters through an environment variable and is validated before reuse: `.github/workflows/publish-fork-npm.yml:86-106`, `.github/workflows/publish-fork-npm.yml:253-277`.

#### FILES READ

(30 entries not reproduced here)

### Orchestrator verification

- P1-1 (unrelated label events give a green `ci-gate` without validating the revision): **mechanism CONFIRMED, downgraded to P2.** The gate has an unconditional `pass` branch for label events other than `full-ci` (ci_gate_decide.py:251-256, asserted by test_ci_gate_decide.py:179-183); it exists so that the labeler's automatic labels do not cancel or re-run real validation. What makes it harmful is a required-status-check rule; the orchestrator read the repository settings: `main` has no branch protection and no rulesets (`gh api .../branches/main/protection` -> 404, rules -> `[]`), and merges are decided from the explicit run id and head sha. It becomes a P1 the day branch protection is enabled. Backlog; recommended fix when that happens: do not create the gate check for ignored label events.
- P1-2 (tier1 Windows omits the exact `internal/agent` package): **PARTLY CONFIRMED, downgraded to P2 (tiering gap, documented).** `internal/agent` is neither a core prefix nor a native-required prefix (ci-policy.toml), the Windows whole-agent job `windows-agent` exists only inside the full-mode shards workflow (ci-tests.yml:486-492; ci-structure.md:19, :35), and the macOS agent step is gated on the agent token. So a PR that changes only Windows-specific agent code or a `*_windows_test.go` file and stays at tier1 gets no Windows agent test run before merge; the push-to-main full run catches it afterwards. This is the trade the operator chose for diff-based selection (full run at the end), but it has a cheap, specific closure: treat a changed `*_windows.go` or `*_windows_test.go` under `internal/agent/` as a reason for full (or for a tier1 Windows agent step). Backlog item with that suggestion.
- Backlog (P2/P3): policy glob `**/` drops the pattern prefix so `internal/skills/**/*` matches unrelated paths (policy.py:94-100); embed names tested without joining the package path (golist.py:115-120); shard coverage check does not prove each group is executed (assign_shards.py:188-204); selector tests do not cover embed ownership, glob isolation, real git collection or cross-package test-import closure (test_ci_plan.py); licenses cache fingerprint ignores source imports (INFERRED); numeric prerelease tags reach the `latest` dist-tag (also reported in W3-04); stale "shadow mode" heading in ci_plan/report.py:35.
- The "found sound" list (fail-closed gate checks, shard assignment validation, full-directive parsing via subprocess output, pinned third-party actions, labeler without checkout) was not re-checked by the orchestrator beyond the cited lines.

---

## W3-08 — Tool layer and security boundary

Reviewer session `rv3-08-tools-security`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
The reviewed boundary has substantial defensive machinery, but I found three P0 policy-bypass paths: argument injection through scoped grep, incomplete enforcement of the linked-worktree git guard, and forbidden writes checked against something other than the final mutation target. These are static-review findings, not executed reproductions. No files were changed; no builds, tests, linters, Go commands, or Rush binaries were run.

#### P0/P1 findings

##### [P0] A scoped grep pattern can launch a ripgrep preprocessor
- Where: `internal/agent/tools/rg.go:62`, `internal/agent/tools/fs_grep.go:123`, `internal/agent/tools/fs_grep.go:325`, `internal/agent/tools/fs_provider_os.go:139`
- Scenario: Ripgrep is installed; Rush’s process working directory contains searchable files and an executable `pre.sh` that writes a marker or modifies another file. Give the model a folder scope granting only `grep`, with command tools disabled. Call `fs_grep` with `pattern` equal to `--pre=./pre.sh`, `literal_text: false`, and an allowed directory as `path`. Ripgrep interprets the purported pattern as its preprocessor option; the directory argument becomes the search expression, and the default search invokes the preprocessor on working-directory files. The preprocessor’s side effects happen even if the search ultimately returns no matches.
- Mechanism: **VERIFIED:** `fsGrepRunItem` forwards the pattern unchanged to `disk.Search` (`fs_grep.go:124-139`), and OSDisk reaches ripgrep (`fs_provider_os.go:139`). The argv builder inserts the model’s pattern directly among options, without `-e` or an option terminator (`rg.go:64-71`). The process is then started without shell-policy checks (`fs_grep.go:325-359`). **INFERRED:** ripgrep’s `--pre` interpretation supplies command execution despite the grep-only capability; this external-program behavior was not exercised.
- Fix idea: Pass the pattern as an explicit `--regexp` operand, and place paths after `--`. Add an integration regression demonstrating that an option-looking pattern cannot execute a preprocessor.
- Confidence: high; VERIFIED argv-injection path, INFERRED execution outcome.

##### [P0] Linked-worktree git protection is neither a complete mutation filter nor a runtime guard
- Where: `internal/agent/agentguard/git_writes.go:13`, `internal/agent/agentguard/git_writes.go:90`, `internal/agent/agentguard/agentguard.go:676`, `internal/agent/tools/bash.go:109`, `internal/agent/tools/bash.go:160`
- Scenario:
  1. In a linked worktree with a shared `main` branch and at least two commits, an auto-approved worker calls Bash with `git update-ref refs/heads/main HEAD~1`. The shared branch moves despite the orchestrator-only git-write rule.
  2. Independently, in a linked worktree containing an uncommitted tracked change, call Bash with the two-line command `printf ok` followed by `git reset --hard HEAD`. The reset bypasses the textual guard and discards the change.
- Mechanism: **VERIFIED:** the git guard rejects only enumerated subcommands; `update-ref` is absent, and unknown subcommands are allowed (`git_writes.go:13-31`, `git_writes.go:138-150`). For the second case, `splitChained` separates semicolons, pipes and `&&`, but not newlines (`agentguard.go:676-727`). Tokenization therefore leaves `printf` as the command head, and `checkGitWritesTokens` returns without inspecting the subsequent git command (`git_writes.go:94-118`). The runtime block functions include canonical shell restrictions and the agent-recursion guard—not `CheckGitWritesArgs` (`bash.go:109-110`, `agentguard.go:169-175`). Thus later shell parsing/expansion does not restore the git-write restriction.
- Fix idea: For linked-worktree execution, enforce a closed, argument-aware set of permitted git operations at the expanded runtime argv boundary. Reuse the shell AST for preflight instead of the separate chain splitter.
- Confidence: high; VERIFIED by code inspection; neither scenario was executed.

##### [P0] Forbidden-write protection can be bypassed through aliases or the download tool
- Where: `internal/agent/tools/forbidden_writes.go:25`, `internal/agent/tools/fs_write.go:71`, `internal/agent/tools/fs_batch.go:303`, `internal/agent/tools/download.go:88`, `internal/agent/tools/download.go:243`
- Scenario:
  1. A workspace contains `protected.txt` and a pre-existing file symlink `alias.txt` pointing to it. The orchestrator sets `RUSH_FORBID_WRITES` to the absolute path of `protected.txt`; the folder scope otherwise grants overwrite access. Calling `fs_write` on `alias.txt` overwrites `protected.txt`.
  2. Without any alias, an auto-approved worker calls `download` using a reachable public resource and sets `file_path` to the exact forbidden path. A successful download replaces the forbidden file.
- Mechanism: **VERIFIED:** forbidden-path normalization performs only absolute-path conversion, lexical cleaning and Windows case folding (`forbidden_writes.go:46-55`). `fs_write` checks the unresolved input (`fs_write.go:71-75`), but the batch runner subsequently resolves the symlink (`fs_batch.go:303`; `fs_scope.go:105-116`) and commits to that different, canonical target (`fs_write.go:287`; `edit.go:284`). No forbidden check follows resolution. Separately, `download` resolves its destination, requests permission and renames over it without calling `CheckForbiddenWrite` anywhere in the implementation (`download.go:88-113`, `download.go:243`).
- Fix idea: Apply the forbidden-write check to the actual resolved mutation target immediately before committing, using the same namespace for blacklist entries and targets. Apply that check to download destinations as well.
- Confidence: high; VERIFIED by code inspection; neither scenario was executed.

#### Backlog (P2/P3)
- [P2] `internal/agent/tools/git_read.go:220` — Git output accumulates in an unbounded `bytes.Buffer`; the display cap is applied only after completion, so large blobs or patches can consume excessive memory (VERIFIED).
- [P2] `internal/agent/tools/write.go:89`, `internal/agent/tools/fs_write.go:219`, `internal/agent/tools/fs_replace.go:184`, `internal/agent/tools/fs_write_lines.go:191` — Full-file mutation snapshots lack the size guard present in legacy edit, exposing large-file memory exhaustion paths (VERIFIED).
- [P2] `internal/agent/tools/write.go:105`, `internal/agent/tools/edit.go:160` — Parent directories are created before permission is requested, so denying a write/create still leaves filesystem changes behind (VERIFIED).
- [P2] `internal/agent/tools/fs_scope.go:155` — Scoped operations resolve names and later reopen them without handle-relative containment; symlink swaps remain an explicitly documented boundary gap, but I did not establish a model-only reproducible escape with command tools absent (VERIFIED).
- [P2] `internal/agent/tools/fs_delete.go:109` — Delete follows symlinks during resolution and removes the resolved regular-file target, rather than the named symlink; this can unexpectedly delete the target within an otherwise permitted scope (VERIFIED).

#### Checked and found sound
- **VERIFIED:** Default fetch/download clients install the SSRF check on the resolved dial address and disable environment proxying, avoiding a separate DNS precheck/rebinding window and the proxy-target mismatch (`internal/agent/tools/ssrf_guard.go:125-170`).
- **VERIFIED:** Production HTTP-tool construction uses guarded defaults unless the operator explicitly enables private-network fetching (`internal/agent/coordinator_tools.go:618-635`, `internal/agent/coordinator_tools.go:720-723`).
- **VERIFIED:** Folder-scoped tool shaping removes legacy filesystem tools, download, repository-wide git reads and MCP access; command tools are removed unless explicitly retained (`internal/agent/coordinator_tools.go:323-342`, `internal/agent/coordinator_tools.go:381-421`).
- **VERIFIED:** Folder-scope roots and requested paths use the same symlink-resolution routine; ordinary existing symlink escapes resolve outside the grant rather than passing on lexical containment (`internal/agent/tools/fs_scope.go:105-116`, `internal/agent/tools/fs_scope.go:161-200`).
- **VERIFIED:** Legacy workspace reads use an `os.Root` anchor, check root identity when opening, and validate regular-file handles (`internal/agent/tools/read_authorization.go:39-59`, `internal/agent/tools/read_authorization.go:131-151`, `internal/agent/tools/regular_open_unix.go:38-61`).
- **VERIFIED:** Restricted-run prefix/exact/glob authorization requires a literal simple command, rejecting assignments, redirections, expansions and control syntax (`internal/permission/runallowlist.go:278-318`, `internal/shell/compound.go:11-39`).
- **VERIFIED:** Background-shell admission reserves active slots under a mutex, output retention is bounded, and ownership checks prevent direct access to another session’s job (`internal/shell/background.go:434-502`, `internal/shell/background.go:166-213`, `internal/shell/background.go:555-563`).

#### FILES READ

(0 entries not reproduced here)

### Orchestrator verification

- P0-1 (scoped grep: pattern becomes a ripgrep option): **CONFIRMED** as an argv-injection path. `buildRgSearchCmd` builds `rg --json -H -n -0 <pattern> [...] <path>` with the model's pattern as a bare positional and no `-e`/`--` (internal/agent/tools/rg.go:64-71), and `fs_grep` forwards the pattern unchanged (fs_grep.go:124-139). A pattern such as `--pre=<command>` is parsed by ripgrep as its preprocessor option. That ripgrep then runs the command is ripgrep's documented behaviour and was NOT exercised by the orchestrator (INFERRED). It matters in the folder-scoped, grep-only configuration whose stated invariant is "no command execution": a model that can also write a file inside the scope could run it with `--pre=sh`. Kept at P0 for that configuration; the fix is two tokens (`-e <pattern>`, `--` before the path) plus a regression test, and the other rg call sites need the same check.
- P0-2 (linked-worktree git guard): **CONFIRMED, rated P1** (a guard rail against a cooperative-but-sloppy model, not a barrier against a hostile one). The guard is a deny-list of git subcommands (agentguard/git_writes.go:13-31) that omits ref writers such as `update-ref` and `symbolic-ref`, and `splitChained` splits on `;`, `|`, `&&`, `||` but not on a newline (agentguard.go:676-727), so in a multi-line command only the first segment's head is examined (e.g. `cd sub` newline `git reset --hard`). Multi-line shell commands are ordinary model output, so the bypass is reachable without any malice. Fix: split on newlines (or reuse the shell AST used elsewhere), complete the deny-list, preferably switch the linked-worktree mode to an allow-list of read-only subcommands.
- P0-3 (forbidden-write checks bypassed through symlink alias or the download tool): **CONFIRMED, downgraded to P2.** `CheckForbiddenWrite` compares lexically cleaned absolute paths only (forbidden_writes.go:46-55) and is called by edit/write/multiedit/fs_write/fs_replace/fs_write_lines but not by `download` or by the resolved target; the feature is the orchestrator's blacklist of shell stdout-redirect targets (its own error text says so), i.e. protection against accidental clobbering of an output file, not a security boundary. Backlog with the fix idea (check the resolved target just before commit and in download).
- Backlog (P2/P3): unbounded buffer in git_read output; missing size guard on full-file mutation snapshots in the newer write tools; parent directories created before the permission prompt; scoped operations re-open resolved names without handle-relative containment (documented boundary gap); `fs_delete` removes the symlink target rather than the named link.

---

## W3-06 — Coordinator and run-loop concurrency

Reviewer session `rv3-06-coordinator-concurrency`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
The mailbox’s core ownership transitions are generally careful, but I found **three P1 failure scenarios** involving interrupt recovery and multi-leg dispatcher accounting. All are **VERIFIED by static code-path inspection**, not runtime reproduction. No files were changed; no builds, tests, linters, Go commands, or Rush binaries were run.

#### P0/P1 findings

##### [P1] An interrupt before the first assistant row can silently discard the accepted replacement
- Where: `internal/agent/agent_turn_failure.go:83`, `internal/agent/coordinator_interrupt.go:642`, `internal/agent/agent_ownership.go:430`, `internal/agent/queued_call_resolution.go:55`
- Scenario:
  1. A web turn has persisted its user message and entered its first `PrepareStep`, but has not created the assistant row.
  2. Hold the SQLite writer in another operation so `DrainPendingInjects` blocks at `internal/agent/agent_turn_step.go:72`.
  3. Submit interrupt-and-send. The replacement is recorded successfully and the handler reports `"queued"` (`internal/server/handlers_agent.go:338–347`).
  4. Cancellation releases the blocked prepare operation with `context.Canceled`.
- Mechanism: The replacement owns a construction scope through an `onQueueResolved` callback that only closes that scope (`internal/agent/coordinator_interrupt.go:642–643`). However, `handleStreamFailure` returns immediately when no assistant exists, **before** its cancel-recovery drain (`internal/agent/agent_turn_failure.go:86–87`, `303–306`). The dispatcher consequently exits and abandons the replacement. `restartOrphanedWithRetry` passes it through `splitCallsWithoutWaiters`, which resolves and **removes every hook-bearing call instead of durably enqueueing it** (`internal/agent/agent_ownership.go:429–430`; `internal/agent/queued_call_resolution.go:55–60`). This callback is cleanup-only: no waiter or caller receives the rejection. The replacement prompt was never persisted or executed.
- Fix idea: Run cancel-replacement recovery even when `currentAssistant` is nil. Also distinguish cleanup-only scope callbacks from actual queued-result waiters so orphan recovery does not silently discard interrupt-and-send calls.
- Confidence: high; **VERIFIED** static path. The required blocked transaction and cancellation ordering were not executed.

##### [P1] A failed replacement can terminally delete an unrelated, never-executed durable call
- Where: `internal/agent/mailbox_interrupt.go:294`, `internal/agent/agent_run.go:673`, `internal/agent/agent_ownership.go:464`, `internal/session/run_queue_entry_exec.go:728`
- Scenario:
  1. The pump leases durable call **D** and its `Run` acquires ownership.
  2. Before the first generation is armed, a web interrupt-and-send supplies replacement **R**.
  3. The dispatcher selects R and puts D at the head of `submitted`.
  4. R creates its user/assistant rows, then receives a terminal provider error, such as HTTP 400.
- Mechanism: `reclaimReplacementOrKeep` always requeues the displaced original call, including a `FromDurableQueue` call (`internal/agent/mailbox_interrupt.go:294–307`). R’s post-message error becomes `ErrCallAlreadyAttempted` and returns without draining queued work (`internal/agent/agent_turn_failure.go:73–75`, `303–312`). During abandonment, D is removed from the mailbox but deliberately not re-enqueued because it already has a durable row (`internal/agent/agent_ownership.go:451–467`). The dispatcher returns **R’s error** to the execution of D. The pump interprets that `AlreadyAttempted` marker as evidence about **D** and terminally removes D’s leased row (`internal/session/run_queue_entry_exec.go:728–736`). D never reached message creation or the provider.
- Fix idea: Associate the pump’s completion/error classification with its leased logical call, rather than the last leg executed by the dispatcher. If a displaced durable call never runs, return a not-executed outcome that preserves its row.
- Confidence: high; **VERIFIED** static path. The pre-first-generation replacement/requeue behavior is also explicitly exercised by `internal/agent/run_interturn_interrupt_test.go:628–693`, but that test was only read.

##### [P1] Auto-compaction completes queued-call ownership before its continuation runs
- Where: `internal/agent/agent_turn.go:858`, `internal/agent/agent_run.go:746`, `internal/agent/queued_call_scope.go:163`, `internal/agent/coordinator_run_await.go:42`
- Scenario:
  1. A pinned, scoped call B queues behind another turn, or B is an interrupt-and-send replacement with its own construction scope.
  2. B executes a tool-using step whose token usage triggers automatic summarization.
  3. Summarization succeeds and `runTurn` returns an internal continuation of B.
  4. The continuation calls another pinned work tool.
- Mechanism: The continuation is a shallow copy of B, preserving its tools, options/scope and resolution callback (`internal/agent/agent_turn.go:858–877`). Nevertheless, **every loop iteration** invokes `turnEndRelease` and `onQueueResolved` before advancing to `next` (`internal/agent/agent_run.go:746–762`):
  - A retained queued B closes its construction scope and clears its retained marker (`internal/agent/queued_call_scope.go:163–171`).
  - An interrupt replacement’s callback closes its independently owned scope (`internal/agent/coordinator_interrupt.go:642–643`).
  - A call awaited through `runAwaitingAdmission` publishes completion with the compaction leg’s `(nil, nil)` result, before its continuation finishes (`internal/agent/coordinator_run_await.go:42–57`).

  With no remaining root/execution owner retaining B’s construction, closing the scope prunes the registry (`internal/agent/disk_provider_capability.go:97–112`, `222–229`, `275–284`). The continuation still uses B’s old pinned tools and fails when acquiring its next root against the missing construction (`internal/agent/batch_ingress.go:217–228`). For awaited calls, the caller can additionally receive an empty successful result while the continuation is still running.
- Fix idea: Treat compaction continuation as another leg of the same logical call: defer its build-scope release and result callback until the final continuation leg completes. Do not apply that deferral to a genuinely different queued call.
- Confidence: high; **VERIFIED** static path. No end-to-end compaction scenario was run.

#### Backlog (P2/P3)
- [P2] `internal/agent/agent_run.go:718` — Promoted calls retain the dispatcher’s original context values rather than rebinding their own options/credentials/origin; context-reading batch paths can therefore disagree with the promoted call’s pinned fields (INFERRED).
- [P2] `internal/agent/agent_turn_failure.go:134` — Missing-tool-result recovery searches the entire session by tool-call ID; an earlier result with a reused ID suppresses recovery for the current occurrence (VERIFIED).
- [P2] `internal/agent/coordinator_interrupt.go:654` — Interrupt-and-send routes through `currentAgent`, unlike cancel/inject/wake’s `agentFor`; interrupting a live delegated child misses its actual mailbox and falls back to durable execution (VERIFIED).
- [P2] `internal/agent/agent_run.go:396` — The workspace guard applies a deadline only when the raw override is positive, leaving default-configured agents’ extra preamble read unbounded despite the bounded-preamble intent (VERIFIED).
- [P3] `internal/filelock/file_lock.go:38` — The comment calls unlinking an acquired POSIX lock file harmless, although reopening the removed pathname can create an independently locked inode; the implementation correctly leaves the file in place (VERIFIED).
- [P3] `docs/async-invariants.md:97` — Multiple invariant anchors still describe the removed `workLedger`/`async_jobs` mechanism, whereas current run-loop accounting uses batch facts; the registry is not a reliable current implementation map (VERIFIED).

#### Checked and found sound
- **VERIFIED:** Normal submit versus final release is serialized through the same mailbox mutex; `mbReleasing` prevents a new in-process owner during lock teardown, and raced-in work is returned as orphaned rather than executed without a lock (`internal/agent/mailbox_ownership.go:229–401`).
- **VERIFIED:** Ordinary busy submission from the durable queue does not create a second mailbox copy (`internal/agent/mailbox_ownership.go:69–83`).
- **VERIFIED:** Epoch-checked abandonment cannot pop a newer owner’s submitted queue (`internal/agent/mailbox_queue.go:174–200`).
- **VERIFIED:** Lock-acquisition errors fail closed before admission setup, rather than running without the inter-process guard (`internal/agent/agent_run.go:494–575`).
- **VERIFIED:** Drain build failures for unavailable process-bound disk or credential bindings clear periodic rechecks without settling the underlying debt (`internal/agent/coordinator_wake.go:88–96`).
- **VERIFIED:** CLI scope reads completion holds and running work before reaction debt, preserving the intended completion-to-debt handoff ordering (`internal/agent/coordinator_reaction_source.go:215–249`).
- **VERIFIED:** Watchdog cancellation precedes the deferred watchdog join; interrupt tickers likewise cancel before joining (`internal/agent/agent_turn.go:629–633`; `internal/agent/coordinator_run_queue_call.go:472–480`).
- **VERIFIED:** Host-lock probes distinguish genuine contention from unknown I/O/permission failures (`internal/filelock/host_probe.go:54–82`).
- **VERIFIED:** Heartbeat flushes clear dirty state only when the captured generation still matches, avoiding acknowledgment of newer mutations (`internal/heartbeat/writer.go:113–117`).

#### FILES READ

(0 entries not reproduced here)

### Orchestrator verification

- P1-1 (interrupt before the first assistant row discards the accepted replacement): **NOT independently re-verified by the orchestrator** (cited code paths agent_turn_failure.go:83-87/:303-306, coordinator_interrupt.go:642, agent_ownership.go:429-430, queued_call_resolution.go:55-60 were not read). The scenario needs a blocked SQLite writer plus an interrupt landing in the pre-assistant window. Treated as a P1 candidate: a deterministic reproduction test comes first; if it cannot be made deterministic the finding drops to P2 under the Stop Rule.
- P1-2 (a failed replacement terminally deletes an unrelated, never-executed durable call): **NOT independently re-verified** (mailbox_interrupt.go:294-307, agent_ownership.go:451-467, run_queue_entry_exec.go:728-736 not read). Same handling: reproduction test first (pump leases D, interrupt-and-send R before the first generation, R fails with a terminal provider error; D's row must survive).
- P1-3 (auto-compaction completes queued-call ownership before its continuation runs): **PARTLY CONFIRMED.** The dispatcher loop calls `turnEndRelease(&call, originCallOpts)` and `call.onQueueResolved(result, err)` on EVERY iteration, before `call = next` (agent_run.go:746-762), and a compaction continuation is returned as `next` with `hasNext=true` (agent_turn.go:858-877), so the release and the resolution callback do run between the compaction leg and its continuation. That the continuation then fails on a pruned construction scope, and that an awaited caller sees an empty success, are consequences the orchestrator did NOT trace (INFERRED). The existing comment at coordinator_run.go:230-235 already names a related accepted consequence ("stale tools fail closed at the executor"). Reproduction test first.
- Backlog (P2/P3): promoted calls keep the dispatcher's original context values (INFERRED); missing-tool-result recovery searches the whole session by tool-call id; interrupt-and-send uses `currentAgent` where cancel/inject/wake use `agentFor` (a live delegated child's mailbox is missed); workspace-guard deadline applies only when the raw override is positive; filelock comment about unlinking is wrong; docs/async-invariants.md still anchors invariants to the removed `workLedger`/`async_jobs` mechanism.

---

## W3-10 — Persistence (db, session, jobbatch, migrations)

Reviewer session `rv3-10-persistence`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
VERIFIED by static inspection: the reviewed persistence paths generally use strong transaction and identity fences, but I found two P1 defects: a refused migration can permanently suppress legacy import, and forking a compacted session loses its summary boundary and changes provider-facing history. No P0 found. No files changed; no builds, tests, linters, Go commands, or Rush binaries were run.

#### P0/P1 findings

##### [P1] A migration-prohibited open leaves a destination that permanently suppresses legacy import
- Where: `internal/db/connect.go:293`, `internal/db/connect.go:301`, `internal/db/migrate_policy.go:177`, `internal/db/legacy_import.go:50`
- Scenario:
  1. A data directory contains a populated legacy `rush.db`, but no `rush_store_v1.db`.
  2. A development build in a linked worktree opens that shared directory. This passes `WithMayMigrate(false)` through `internal/cmd/root.go:510`.
  3. Startup refuses the pending migration.
  4. Subsequently start a migration-authorized binary against the same directory.
  5. It creates a fresh store-v1 schema without importing the legacy sessions. Future startups continue selecting that empty/new store.
- Mechanism: The prohibited attempt skips `ImportLegacyStore`, but still opens the destination using a create-capable SQLite connection (`internal/db/connect.go:295–301`, `internal/db/connect_modernc.go:24`). Migration refusal happens afterward. The existing test explicitly recognizes that this leaves a destination file without a sessions table (`internal/db/connect_import_test.go:65–85`). On the authorized retry, the importer treats **any existing destination** as authoritative and returns immediately (`internal/db/legacy_import.go:50–53`); normal migration then initializes that file. There is no validation that the existing destination represents a completed import or initialized store.
- Fix idea: Refuse a migration-prohibited first open before creating the destination. Make destination initialization/publication share the import critical section, and distinguish an initialized destination from an empty artifact left by an unsuccessful open.
- Confidence: high; **VERIFIED** code path and concrete two-startup scenario. Not executed. The legacy source remains intact, so this is history becoming inaccessible through normal startup, not physical destruction of the source.

##### [P1] Forking a compacted session loses the summary boundary and replays its summary as assistant output
- Where: `internal/session/session_fork.go:106`, `internal/session/session_fork.go:196`, `internal/db/sql/sessions.sql:34`, `internal/agent/agent_prompt.go:336`
- Scenario:
  1. Compact an existing conversation successfully. The session now points at an assistant-role summary message.
  2. Fork it using the web fork action or `rush sessions fork`.
  3. Continue the fork with another prompt.
  4. The fork sends the summary as prior **assistant output**, whereas continuing the source sends it as **user-role context**. Any retained, non-pinned rows before the source’s summary boundary are also replayed by the fork instead of excluded.
- Mechanism: Compaction persists `summary_message_id` (`internal/agent/agent_compaction.go:520`). The history reader uses that pointer to discard superseded history, preserve preceding pinned messages, and convert the summary to user role (`internal/agent/agent_prompt.go:336–355`). Fork creates a session with a NULL summary pointer and never populates a replacement (`internal/db/sql/sessions.sql:34`; `internal/session/session_fork.go:106–168`). It copies messages with new IDs but preserves their stored roles (`internal/session/session_fork.go:196–209`). Thus copying `IsSummaryMessage` does not restore the behavior: `Message.ToAIMessage` dispatches on role, not that flag (`internal/message/content.go:626–699`). The ordinary turn loads history through this reader (`internal/agent/agent_turn.go:449`).
- Fix idea: Track old-to-new message IDs during the clone and set the fork’s summary pointer to the copied summary when it falls within the selected prefix. Preserve the copied messages’ pin state so the existing boundary logic retains the same pinned context.
- Confidence: high; **VERIFIED** provider-history transformation from the code. Not executed against a provider.

#### Backlog (P2/P3)
- [P2] `internal/session/session_fork.go:197` — copied completed assistant messages retain Finish parts but lose `finished_at`, because `CreateMessage` cannot insert it; SQL deletion treats them as streaming, while Go `IsFinished` treats them as finished (`internal/db/sql/messages.sql:144`, `internal/message/content.go:345`) (**VERIFIED**).
- [P2] `internal/db/sql/files.sql:49` — `ListLatestSessionFiles` computes maxima across all sessions before filtering the requested session; a newer snapshot in session B hides session A’s latest snapshot, and independent version/time maxima can select no row at all; generated SQL has the same defect (`internal/db/files.sql.go:176`) (**VERIFIED**).
- [P2] `internal/session/session_cost_subtree.go:50` — `ResetCostBase` reads spent and writes the base in separate statements; two concurrent resets can write bases in reverse order and resurrect already-reset budget, despite the single-connection atomicity claim at line 46 (**VERIFIED** at service level; production reset admission limits reachability).
- [P2] `internal/db/connect.go:345` — read-pool fallback checks only `sql.Open` errors, not whether the lazy connection actually opens; an initial reader connection failure is returned to read callers instead of activating the documented writer fallback (`internal/db/connect_modernc.go:57`) (**VERIFIED**).
- [P2] `internal/session/batch_consumers.go:85` — ordinary message deletion does not invoke the delivery-reconciliation helper, leaving delivered batch rows pointing at deleted notice messages; the examined delete handler and message service only delete and publish (`internal/server/handlers_messages.go:157`, `internal/message/message.go:338`) (**VERIFIED**).
- [P3] `internal/session/session_update.go:329` — `Rename` promises not to touch `updated_at`, but the unconditional session UPDATE trigger changes it (`internal/db/migrations/20261022000001_store_v1_initial.sql:49`) (**VERIFIED**).
- [P3] `internal/db/migrate_policy.go:168` — the unknown-version comment describes opening “read-only,” but that branch returns the already-opened writer without changing its access mode (`internal/db/connect.go:301–328`) (**VERIFIED**).

#### Checked and found sound
- **VERIFIED:** writer transactions request `BEGIN IMMEDIATE`, avoiding deferred read-to-write upgrades; WAL, foreign keys, and a 30-second busy timeout are configured (`internal/db/connect_modernc.go:17–24`, `internal/db/connect.go:22–30`).
- **VERIFIED:** batch projection, root sequence advancement, selected notices, and runner checkpoints commit together; missing or stale fenced updates abort the transaction (`internal/session/batch_store.go:244–329`).
- **VERIFIED:** batch announcement and notice delivery pair their election with history insertion in one transaction and publish only after commit (`internal/session/batch_ack.go:25–50`, `internal/session/batch_pull.go:190–234`).
- **VERIFIED:** run-queue leasing is transactional; renew/ack/nack mutations are fenced by current lease owner (`internal/session/session_runqueue.go:347–430`, `internal/db/sql/run_queue.sql:47–105`).
- **VERIFIED:** orphan-outbox transfer inserts into the run queue and removes the source row within one transaction (`internal/session/session_orphan_outbox.go:65–113`).
- **VERIFIED:** legacy import builds a private destination, copies carried rows transactionally, verifies row values and integrity, checkpoints, and publishes by rename; future legacy migration versions are refused (`internal/db/legacy_import.go:109–146`, `internal/db/legacy_import.go:311`, `internal/db/legacy_import.go:408–428`, `internal/db/legacy_import.go:713–747`).
- **VERIFIED:** wake-schedule claim and occurrence firing use writer transactions; occurrence advancement and durable notice insertion commit together (`internal/session/wake_schedule_store.go:257–304`, `internal/session/wake_schedule_fire.go:62–128`).
- **VERIFIED:** project registration holds a cross-process sidecar lock across load/mutate/save and uses atomic-file publication (`internal/projects/projects.go:134–194`, `internal/projects/projects.go:108`).

#### FILES READ

(112 entries not reproduced here)

### Orchestrator verification

- P1-1 (a migration-prohibited open leaves a destination that permanently suppresses legacy import): **CONFIRMED** in the code path. With `MayMigrate=false` the import is skipped (connect.go:287-291) but `openDB(dbPath)` and `PingContext` still run against the store-v1 destination (:293-301), which SQLite creates; the existing test only checks the case "if sqlite created the empty file on open" (connect_import_test.go:80-85). On the next authorised start `ImportLegacyStore` returns `Noop` as soon as the destination file exists (legacy_import.go:50-52) and normal migration initialises a fresh store, so the legacy history is left behind (the legacy file itself stays intact). Trigger: a populated legacy `rush.db`, no `rush_store_v1.db`, and the FIRST open done by a non-migrating binary (the linked-worktree dev build path). Narrow in practice for an already-migrated machine, but the result is history that normal startup never shows again; the fix is small (refuse before creating the destination when migration is not allowed). Task created.
- P1-2 (forking a compacted session loses the summary boundary): **CONFIRMED.** The fork copies `IsSummaryMessage` and `Hidden` per message (session_fork.go:205-206) but the new session row is created with `summary_message_id = null` (sessions.sql:34), while the history reader only treats a message as a summary through that pointer, converts it to user role and drops what precedes it (agent_prompt.go:336-358). In the fork the summary therefore goes to the provider as assistant output and nothing is excluded. This interacts with W3-03 P1-1 (the silent compaction boundary): both concern what the history looks like after a compaction, so they are one work package.
- Backlog (P2/P3): fork drops `finished_at` of copied assistant messages; `ListLatestSessionFiles` computes maxima across all sessions before filtering; `ResetCostBase` is read-then-write across statements; read-pool fallback checks only `sql.Open` errors; ordinary message deletion does not reconcile delivered batch rows; `Rename` touches `updated_at` through the trigger; migrate-policy comment says "read-only" for a writer.

---

## W3-09 — Providers, network, OAuth refresh, CLI providers

Reviewer session `rv3-09-providers-network`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
The reviewed code contains substantial defensive handling, but I found one **VERIFIED P1** in peak-hours enforcement and one **INFERRED P1** in stall recovery that needs targeted reproduction before acceptance. Additional issues belong in the backlog. This was a read-only review: no files changed, and no builds, tests, linters, Go commands, or Rush binaries ran. The installed Fantasy module cache did not contain the `v0.45.5` required by `go.mod:9`; I did not read the mismatched version.

#### P0/P1 findings

##### [P1] Mid-turn peak-hours enforcement checks the agent’s original provider, not the executing provider
- Where: `internal/agent/coordinator_tools.go:137`, `internal/agent/agent_turn.go:302`, `internal/agent/agent_turn_peakhours.go:96`, `internal/agent/agent_turn_step.go:795`
- Scenario: Configure the default Smart model on provider A, and run a session with a model override on provider B. Both providers are initially outside peak hours. While B streams, change B’s peak-hours window to include the current time. The running B request is not stopped. Conversely, putting only A into peak hours aborts the B request and reports A’s refusal.
- Mechanism: The callback installed when the shared session agent is constructed closes over `smart.ModelCfg.Provider` from that construction (`coordinator_tools.go:137–139`). A turn can execute a different model through `call.SmartModel`, which `resolveTurnConfig` explicitly prefers (`internal/agent/agent.go:483–485`). Neither the background watcher nor the step-boundary check supplies that executing provider; both invoke the construction-time callback. `UpdateModels` changes model fields without replacing this callback (`internal/agent/coordinator_models_build.go:238–242`).
- Fix idea: Make the live peak-hours checker accept the executing provider ID, and bind each turn’s watcher and step check to `ts.smartModel.ModelCfg.Provider`. Preserve the per-call bypass.
- Confidence: high; **VERIFIED** by the construction, override, and check call paths. Runtime reproduction was not performed.

##### [P1] Stall recovery restarts the entire agent conversation after earlier tools have executed
- Where: `internal/agent/agent_turn.go:798–814`, `internal/agent/turn_stall_policy.go:64–80`, `internal/agent/turn_stall_policy.go:130–150`
- Scenario: Use a provider fixture that, when the prompt contains no tool result, requests a non-idempotent MCP operation such as incrementing an external counter. After receiving its result, have the next model step emit reasoning continuously for over ten minutes without text. Set `TurnStallTimeout` to ten minutes and keep the idle timeout longer, or keep reasoning deltas frequent enough to prevent idle expiry. The stall policy cancels this later request, then the retry invokes the whole agent stream again with the original history. A provider following the same rule requests the increment a second time.
- Mechanism: The retry closure surrounds `agent.Stream`, not one model request. Every re-entry passes the same original `history`, `files`, and prompt through `ts.streamCall` (`agent_turn.go:802`); completed steps are not added to those retry inputs. The policy explicitly advances its retry counter and cancels the attempt, and `runStallRetried` re-enters on that cancellation. The retry callback merely sets `retryStale`; it does not construct a continuation containing earlier tool results. Reasoning deltas keep byte activity alive without advancing the durable-progress clock (`internal/agent/agent_turn_stream.go:376–379`).
- Fix idea: Retry only the stalled provider step with its complete current conversation, or end this attempt and use a continuation built from persisted history. Do not restart the original agent stream after completed tool execution.
- Confidence: medium; **INFERRED**. The whole-stream re-entry and unchanged inputs are verified locally, but the final replay behavior depends on Fantasy `v0.45.5`’s per-`Stream` state initialization, whose matching source was unavailable.

#### Backlog (P2/P3)
- [P2] `internal/agent/agent_usage.go:87–94`, `internal/agent/agent_turn_step.go:766–767` — `MaxTokens` compares the latest prompt/completion snapshot, not cumulative run consumption; two steps reporting 700 and 800 tokens remain below a 1,000-token cap despite consuming 1,500 tokens, and compaction can reduce the measured quantity further (VERIFIED).
- [P2] `internal/agent/cliprovider/provider_parsing.go:203`, `internal/agent/agent_usage.go:91` — Gemini’s larger `TotalTokens`, explicitly preserved because it includes otherwise-unitemized thinking, is ignored by the token-cap calculation (VERIFIED).
- [P2] `internal/agent/agent_cache_keepalive.go:302–314`, `internal/session/session_update.go:72–80` — a successful, already-paid keep-alive response is omitted from the cost ledger when its cost would cross the cap; the existing test explicitly expects this undercount at `internal/agent/agent_cache_keepalive_cost_test.go:531–534` (VERIFIED).
- [P2] `internal/agent/agent_cache_keepalive.go:249–308` — detached keep-alive requests check budget but not current peak-hours policy before sending another paid request (VERIFIED).
- [P2] `internal/agent/agent_turn_checkpoint.go:134` — every checkpoint timer tick marks durable progress before checking for changed content or a successful write, allowing an unchanged checkpoint to postpone stall detection (VERIFIED).
- [P2] `internal/agent/cliprovider/provider_stream_session.go:118–125`, `internal/agent/cliprovider/provider_stream_session.go:159–162` — all raw output and all parsed lines are retained for the stream’s lifetime, duplicating output without a total bound even after the iterator consumes it (VERIFIED).
- [P2] `internal/agent/cliprovider/provider_stream.go:702–714`, `internal/agent/agent_turn_stream.go:454–486` — MCP execution notifications emit only tool-input callbacks, while watchdog pausing begins only in `onToolCall`; a long synchronous CLI MCP operation can therefore be treated as provider silence (INFERRED).
- [P2] `internal/agent/cliprovider/provider_stream.go:735` — debug logging writes complete raw CLI events without requiring the separate raw-prompt opt-in, exposing echoed user messages and tool content to logs (VERIFIED).
- [P2] `internal/log/http.go:212–225` — HTTP logging writes the raw request URL; credentials in a configured endpoint’s query or userinfo are not redacted by the header/body sanitizers (VERIFIED).
- [P2] `internal/agent/coordinator_providers_auth.go:165–179`, `internal/agent/coordinator_run.go:542–544` — the 401 retry does not reuse `createdUserMessageID` before its second execution, unlike the later transient retry path, so an initial 401 can persist the user prompt twice (VERIFIED).
- [P2] `internal/agent/cliprovider/provider_prompt.go:119–124`, `internal/agent/cliprovider/provider_stream.go:744` — resume stores the pre-request prefix hash and never advances it to include the completed exchange; an ordinary next user turn changes the prefix and starts a fresh CLI session instead (VERIFIED).
- [P2] `internal/agent/cliprovider/mcpserver_tools.go:598–608` — legacy `Read(start_line=10,end_line=20)` slices from line 10 and then takes 20 remaining lines, rather than ending at original line 20 (VERIFIED).
- [P3] `internal/config/peak_hours.go:79` — constructing an end time by adding elapsed minutes to midnight can report the wrong reopening wall time on daylight-saving transition days (VERIFIED).

#### Checked and found sound
- **VERIFIED:** Model-pair construction preserves Smart/Fast ordering and uses one provider-config snapshot for each resolution (`internal/agent/coordinator_models.go:203–256`, `internal/agent/coordinator_models_build.go:80–170`).
- **VERIFIED:** A failed required Claude MCP setup returns before launching the CLI, with cleanup on the configuration-creation failures examined (`internal/agent/cliprovider/provider_stream.go:129–161`).
- **VERIFIED:** The explicit tool bridge validates the full advertisement before registering callbacks, dispatches canonical pinned names, and does not substitute legacy execution after a bridge error (`internal/agent/cliprovider/tool_bridge.go:78–112`).
- **VERIFIED:** Bridge request contexts retain request cancellation while resolving identity and policy values from the trusted invocation (`internal/agent/cliprovider/tool_bridge.go:69–101`).
- **VERIFIED:** OAuth bearer retries require a changed access token and a replayable request body, and close the rejected response before resending (`internal/agent/oauth_bearer_transport.go:99–135`).
- **VERIFIED:** The examined refresh paths serialize refresh attempts and use credential comparisons before publication; API-key template refresh also compares both template and prior resolved value (`internal/config/store_oauth_provider.go:82–124`, `internal/config/store_oauth.go:178–197`, `internal/config/store_oauth.go:486–503`).
- **VERIFIED:** Custom network transports bound connect/handshake phases rather than imposing a fixed lifetime on established inference streams; successful CONNECT handoff clears its provisional deadline (`internal/nettransport/client.go:110–138`, `internal/nettransport/proxy.go:178–195`).
- **VERIFIED:** The debug HTTP retry transport does not blindly retry ordinary completion POSTs (`internal/log/http.go:81–88`, `internal/log/http.go:153–159`).
- **VERIFIED:** Watchdog hard-cap enforcement is explicitly conditional on a configured cap and is checked both with and without tools in flight (`internal/agent/stream_watchdog.go:367–373`, `internal/agent/stream_watchdog.go:428–442`).

#### FILES READ

(102 entries not reproduced here)

### Orchestrator verification

- P1-1 (mid-turn peak-hours enforcement checks the agent's construction-time provider): **CONFIRMED.** `PeakHoursCheck` is a closure over the `smart` model captured when the session agent is built (coordinator_tools.go:137-139); the turn resolves its own model through `resolveTurnConfig(call)` which prefers `call.SmartModel`, and the watcher and step-boundary check call the argument-less `peakHoursCheck()` (agent_turn_peakhours.go:90-96, agent_turn_step.go:795). So a session running on provider B is aborted when only the default provider A enters peak hours, and is not stopped when B does. Wrong result in a documented operator feature, reachable with a per-session model override on another provider. Task created (small change: pass the executing provider id).
- P1-2 (stall recovery restarts the whole agent conversation after tools have run): **NOT independently re-verified; INFERRED by the reviewer** (the whole-stream retry shape is stated as verified locally, the replay result depends on fantasy v0.45.5's per-`Stream` state). Note for the record: the reviewer did not find fantasy v0.45.5 in its module cache; it is present in the orchestrator's module cache, so the follow-up can read the real source. A reproduction fixture comes first (non-idempotent tool called twice after a stall retry); without it this stays at P2.
- Backlog (P2/P3): `MaxTokens` cap compares the latest snapshot, not cumulative use; Gemini's larger `TotalTokens` ignored by the cap; a paid keep-alive response omitted from the cost ledger when it would cross the cap; keep-alive does not re-check peak hours; checkpoint ticks mark durable progress before checking for change (postpones stall detection); cliprovider retains all raw output for the stream lifetime; MCP execution notifications do not pause the watchdog (INFERRED); debug logging writes full raw CLI events and raw request URLs (credentials in a query or userinfo are not redacted); a 401 retry does not reuse `createdUserMessageID`, so the first prompt can be persisted twice; CLI resume prefix hash never advances; legacy `Read(start_line,end_line)` slicing; DST edge in peak-hours end time.

---

## W3-11 — HTTP/WebSocket server and React UI

Reviewer session `rv3-11-server-web`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
Source-only review found **three P1 defects** in reconnect recovery, queued-message delivery, and attachment submission. No P0 was established. The inspected authentication and origin-check paths have substantive safeguards, but the UI’s recovery logic can leave persisted output missing or automatically resend an ambiguously delivered instruction. No files changed; no builds, tests, linters, Go commands, or Rush binaries were run.

#### P0/P1 findings

##### [P1] Ambiguously delivered queued messages are automatically sent again
- Where: `web/src/store_commands.ts:218-237`, `web/src/ws.ts:236-239`, `web/src/useWS.ts:414-443`, `web/src/store.ts:763-772`
- Scenario: While a session is running, queue an instruction and click its **Send now** action. The server persists the injection, but the connection drops before its acknowledgment reaches the browser. Let the turn finish before reconnecting. The browser restores the instruction with “May already be sent,” then the reconnect’s `agent_busy=false` event automatically submits it again as `send_message`, without operator confirmation.
- Mechanism: Injection persists the user message before returning (`internal/agent/agent_control.go:124-135`), and the server acknowledges only after that operation (`internal/server/handlers_agent.go:389-395`). A disconnect rejects the pending request as ambiguous, but the catch restores it to the ordinary automatic-delivery queue. `dequeueAllMessages` includes every entry regardless of its error, and the busy-false handler automatically sends the result. The warning text therefore does not prevent duplicate execution.
- Fix idea: Keep ambiguous deliveries outside the automatic-flush queue, requiring explicit reconciliation or retry. Alternatively, make submissions idempotent using a client-generated delivery ID persisted server-side.
- Confidence: high; **VERIFIED** from the complete persistence, acknowledgment, disconnect, restore, and automatic-flush paths. Not runtime-exercised.

##### [P1] Reconnect never reloads the active locally driven transcript
- Where: `web/src/useWS.ts:98-142`, `web/src/useWS.ts:221-259`, `web/src/useWS.ts:533-538`, `internal/server/hub.go:77-84`, `internal/server/hub.go:285-289`
- Scenario: Open an existing session driven by this web server, then disconnect while its assistant response grows beyond the replay buffer’s 1 MiB per-event ceiling. The response finishes while disconnected. Reconnect with the same session still active and its hash unchanged. The browser receives only the replayable earlier snapshots, not the larger final response, and can remain permanently truncated or show an unfinished message although the DB contains the completed answer.
- Mechanism: Large snapshots are deliberately excluded from replay, with a comment relying on a DB load to recover them. However, `_connected` refreshes sessions/config/live-work but does not call `sendLoadMessages` for the active transcript. The sessions-list handler reloads messages only for externally owned sessions or when changing the active session. Periodic message polling also excludes locally owned sessions. Consequently, neither reconnect nor subsequent ordinary polls repairs this missed final state.
- Fix idea: Reload the active session’s messages on every reconnect, using the existing snapshot/live-event reconciliation machinery. Refresh registered sub-agent transcripts as appropriate rather than relying on bounded replay for authoritative recovery.
- Confidence: high; **VERIFIED** from the replay exclusion and all message-refresh call paths in `useWS.ts`. Not runtime-exercised.

##### [P1] An ordinary oversized attachment silently discards the submitted prompt
- Where: `web/src/components/ChatInput.tsx:670-675`, `web/src/components/ChatInput.tsx:370-385`, `web/src/ws.ts:101-107`, `internal/server/hub.go:18`, `internal/server/server.go:283-310`
- Scenario: Attach a 16 MiB file to an idle session, type an instruction, and press Send over an open socket. Base64 alone expands the file to 22,369,624 bytes, exceeding the server’s 20,971,520-byte WebSocket message limit before JSON overhead. The composer clears the instruction and attachment, but the server never dispatches that submission.
- Mechanism: File selection accepts the file without a size check and encodes it as base64 (`web/src/components/ChatInput.tsx:31-47`). `sendQueued` treats a successful browser `WebSocket.send` as delivery, so the composer immediately clears its state. The server read limit rejects the oversized message; its read loop returns on the error without calling `handleIncoming`. The frame is not retained in the offline outbox because the original send returned true.
- Fix idea: Validate the complete encoded frame size before sending and preserve the draft on refusal. For larger files, use a bounded upload protocol; acknowledgment-based draft retention would also protect against server-side rejection.
- Confidence: high; **VERIFIED** size arithmetic and submission/read-limit paths. Not runtime-exercised.

#### Backlog (P2/P3)
- [P2] `web/src/components/WakeScheduleList.tsx:90-95` — Successful cancellation leaves the confirmation dialog open: success detection requires the row to disappear, but `internal/server/handlers_wake_schedules.go:124-148` includes cancelled rows in the returned snapshot (VERIFIED).
- [P2] `web/src/store_livework.ts:119-122` — Wake-schedule snapshots replace state without request/version ordering; a delayed pre-cancellation GET can overwrite the newer cancellation result (VERIFIED).
- [P2] `web/src/store_commands.ts:224`, `web/src/store.ts:763-772` — Even definitely rejected queued-message actions return to automatic delivery, so a later busy-false poll retries them despite the wrapper’s documented “Never auto-resent” policy at `web/src/store_commands.ts:249-250` (VERIFIED).
- [P3] `web/src/keepAlive.ts:105-111` — Auto-resume installs an anonymous listener without cleanup or an installation guard; each `useWS` remount adds another listener, including StrictMode’s development remount (`web/src/main.tsx:12-15`) (VERIFIED).

#### Checked and found sound
- **VERIFIED:** `/ws` is behind token authentication; cookies, Bearer credentials, and query credentials use constant-time comparison (`internal/server/server.go:154-159`, `internal/server/auth.go:125-139`).
- **VERIFIED:** Browser WebSocket origins are matched against explicit loopback hosts and the bound port, rather than trusting the request’s Host header (`internal/server/server.go:61-81`).
- **VERIFIED:** Login-body parsing is bounded to 4 KiB and checks trailing input as well as the first JSON value (`internal/server/auth.go:60-81`).
- **VERIFIED:** Attachment filenames are reduced to their basename and prefixed before joining the configured attachments directory; the inspected upload path does not directly use an uploaded filename as a destination path (`internal/server/handlers_agent.go:138-157`).
- **VERIFIED:** Hub fan-out and handler admission are nonblocking and bounded; the hub loop owns client-map mutation and broadcast-channel closure (`internal/server/hub.go:187-225`, `internal/server/hub.go:445-504`, `internal/server/hub.go:625-633`).
- **VERIFIED:** Pubsub channel removal/closure takes the write lock while publishers hold the read lock, protecting those sends from concurrent subscriber-channel closure (`internal/pubsub/broker.go:143-154`, `internal/pubsub/broker.go:185-207`).
- **VERIFIED:** Inspected user text, tool output, and diff previews use JSX text rendering; inspected markdown configuration does not enable `rehype-raw` (`web/src/components/Message/TextBlock.tsx:8-12`, `web/src/components/Message/ToolResultBlock.tsx:46`, `web/src/components/Message/EditPreview.tsx:13-17`, `web/src/components/Message/mdPlugins.ts:4-10`).

#### FILES READ

(59 entries not reproduced here)

### Orchestrator verification

- P1-3 (an ordinary oversized attachment silently discards the submitted prompt): **CONFIRMED.** The server read limit is 20 MiB (`maxMessageSize = 20*1024*1024`, hub.go:18; `SetReadLimit` server.go:279); the composer reads the size only for display and has no limit check (ChatInput.tsx grep: `size` used for `formatBytes`), and base64 turns a 16 MiB file into about 22.4 MB. The reviewer's description of the composer clearing its draft after a successful `WebSocket.send` and of the server dropping the frame was not re-read by the orchestrator (INFERRED), but the arithmetic and the missing check are verified. Task created.
- P1-1 (ambiguously delivered queued messages are automatically sent again): **PARTLY CONFIRMED.** `sendQueuedMessageNow` and `interruptAndSendQueuedMessage` put the item back with `restoreQueuedMessage(..., describeSendFailure(err))` on any rejection, including a disconnect after the server persisted the injection (store_commands.ts:218-237); that the restored item is then flushed by the next `agent_busy=false` (store.ts:763-772) and that the "never auto-resent" comment at store_commands.ts:249-250 contradicts it were not read by the orchestrator. Task created with a reproduction-first step.
- P1-2 (reconnect never reloads the active locally driven transcript after a large snapshot was excluded from replay): **NOT independently re-verified** (hub.go replay ceiling and useWS.ts reconnect paths not read). Task created with a reproduction-first step.
- Backlog (P2/P3): wake-schedule cancel dialog stays open; wake-schedule snapshots have no ordering; definitely rejected queued messages also return to auto-delivery; keep-alive installs an anonymous listener per remount.
- The auth/origin/rendering "found sound" entries were not re-checked beyond the cited lines.

---

## W3-12 — Config, CLI surface, sessions family

Reviewer session `rv3-12-config-cli-sessions`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
**VERIFIED by static code tracing:** found two P0 history-loss paths in session maintenance and one P1 configuration-recovery failure. The examined config commit paths have substantial atomicity safeguards, but session deletion/reset admission remains inconsistent. This was a read-only review: no files changed; no builds, tests, linters, Go commands, or Rush binaries run. Runtime reproduction remains unperformed as instructed.

#### P0/P1 findings

##### [P0] Default garbage collection deletes actively resumed sessions
- Where: `internal/cmd/sessions_gc.go:137`, `internal/cmd/sessions_gc.go:158`, `internal/cmd/sessions_gc.go:215`, `internal/cmd/sessions_gc.go:241`; `internal/cmd/sessions_batch_act.go:119`; `internal/session/session_lifecycle.go:128`
- Scenario: Create a session named `ping-investigation`, retain it for more than one hour, then resume it with `rush run --role smart --session ping-investigation ...`. While that run is active, execute ordinary `rush sessions gc` against the same data directory. Assume the resumed session has no batch roots or unreacted notices. GC selects and deletes it, including its existing transcript, despite its live driver and held session lock. An actively resumed orphaned child older than 24 hours takes the same path without needing the `ping-` prefix.
- Mechanism: **VERIFIED:** `classifyForGC` selects old `ping-` sessions regardless of message count, and old orphaned children regardless of activity (`sessions_gc.go:243–250`). The only subsequent protection is `gcDropBatchProtected`, which checks batch roots and delivery debt—not the session lock or durable driver (`sessions_batch_act.go:124–131`). `Sessions.Delete` then deletes messages, files, and the session in a transaction without checking either form of ownership (`session_lifecycle.go:137–150`). CLI runs explicitly publish a durable driver claim, but GC never consults it (`internal/app/app_run_async.go:369–377`).
- Fix idea: Require an exclusive session-maintenance admission before deleting, refusing live drivers and held locks. Revalidate the GC predicate inside the deletion transaction so newly populated sessions cannot be deleted from an old candidate snapshot.
- Confidence: high; **VERIFIED** code path, not runtime-executed.

##### [P0] Plain reset wipes a currently streaming web turn instead of refusing it
- Where: `internal/cmd/sessions_reset.go:78–114`; `internal/cmd/sessions_batch_act.go:67–75`; `internal/session/batch_history_reset.go:82–106`
- Scenario: Start a normal web conversation and let its provider stream a response without launching batch work. While the turn is still active, run `rush sessions reset <id>` **without** `--force` against the same data directory. The reset deletes the conversation, including the in-progress assistant message, while the original turn continues to own its session lock.
- Mechanism: **VERIFIED:** only the `--force` branch acquires and holds the session lock (`sessions_reset.go:78–102`). The shared refusal helper reads full activity facts but rejects only `Facts.Driver != nil`; it ignores a held turn lock (`sessions_batch_act.go:68–75`). Web sends invoke the coordinator directly rather than claiming the CLI external-driver marker (`internal/server/handlers_agent.go:239–247`). Their actual turn owns an OS session lock (`internal/agent/agent_run.go:491–529`). Finally, `BatchResetOwnerHistory` refuses nonterminal batch roots only, then deletes all owner messages (`batch_history_reset.go:93–108`). A streaming turn with neither a CLI driver nor a batch root therefore passes every gate. This contradicts the command’s explicit promise that a session still being worked on is never reset (`sessions_reset.go:29–38`).
- Fix idea: Acquire the session lock for non-force reset too, returning a busy refusal rather than killing its holder. Hold it through the history/counter reset and retain the existing driver and batch-work gates.
- Confidence: high; **VERIFIED** code path, not runtime-executed.

##### [P1] A surviving config writer can invalidate an interrupted migration’s recovery record
- Where: `internal/config/ktav_migration.go:318–337`, `internal/config/ktav_migration.go:193–204`, `internal/config/ktav_migration.go:432–437`, `internal/config/ktav_migration.go:498–500`; `internal/config/store_write.go:249–298`, `internal/config/store_write.go:431–474`
- Scenario:
  1. Process A already has a store loaded against a legacy JSON slot—for example, a previously unrepresentable document kept migration deferred, and the document has since been corrected.
  2. Process B migrates that slot, publishes `rush.ktav`, then crashes before removing `rush.json.ktav-migration`. The JSON source, published Ktav, and valid recovery record remain.
  3. Process A performs a settings write after B’s OS lock is released.
  4. A commits its edit into Ktav. Its reconciliation, and subsequent fresh application loads, fail migration recovery because the recovery record still describes the pre-edit Ktav digest.
- Mechanism: **VERIFIED:** migration persists the record before publishing Ktav and retires it only afterward (`ktav_migration.go:318–337`, `:384`). The generic writer correctly reselects Ktav after acquiring the shared sidecar lock, but does not finish or refuse a pending migration before invoking its mutation (`store_write.go:249–298`). Its successful commit changes the bytes bound by the record. Reload runs pending migration recovery first (`internal/config/store_reload.go:360–372`), whose exact staged-digest check now rejects the legitimate settings edit (`ktav_migration.go:432–437`). `SetConfigFields` logs that reload failure and still returns success after an otherwise successful commit (`store_write.go:471–476`). Fresh `Load` also runs recovery before loading configuration (`internal/config/load.go:68–69`), leaving startup blocked until manual resolution.
- Fix idea: While holding the shared slot lock, complete any pending migration before permitting a mutation, or reject the write without changing bytes until recovery succeeds. Do not let a writer invalidate the record’s authenticated generation.
- Confidence: high; **VERIFIED** code path, not runtime-executed.

#### Backlog (P2/P3)
- [P2] `internal/cmd/providers.go:580–603` — Provider refresh swallows persistence errors, and `--all` returns success even when every refresh fails; automation cannot rely on the exit status (VERIFIED).
- [P2] `internal/cmd/providers.go:472`, `internal/cmd/providers.go:580` — `providers add --local --enable` refreshes models through a helper that always writes global scope, unexpectedly creating a global provider fragment (VERIFIED).
- [P2] `internal/cmd/providers_patch.go:80`, `internal/cmd/providers.go:238` — Provider IDs are concatenated into sjson paths without escaping; a literal dotted ID such as `team.prod` addresses nested objects rather than that provider key (VERIFIED).
- [P2] `internal/cmd/sessions_diff.go:177–194` — Hand-scanned JSON strings are not unescaped and stop at escaped quotes; Windows paths and quoted filenames are reported incorrectly and sent incorrectly to Git (VERIFIED).
- [P3] `internal/cmd/sessions_tree.go:117–140` — A top-level root begins with empty prefix and `isLast=true`, and its children inherit the empty prefix; the text renderer consequently loses tree indentation/connectors throughout that tree (VERIFIED).
- [P2] `internal/cmd/claude_init.go:204`, `internal/cmd/claude_del.go:211` — Legacy-block removal rewrites the remaining user-owned document with truncating `os.WriteFile`; interruption during that write can damage unrelated retained content (VERIFIED).

#### Checked and found sound
- **VERIFIED:** Generic config writes hold a cross-process sidecar lock, reselect the active slot after waiting, and reread the mutation base under that lock (`internal/config/store_write.go:241–298`, `:376–388`). The previously reported stale-slot write issue is not simply assumed present.
- **VERIFIED:** Unix config publication stages and syncs bytes before rename, then syncs the pinned parent; Windows publication keeps the staged handle pinned and distinguishes committed/uncertain outcomes (`internal/config/config_commit_unix.go:77–213`; `internal/config/config_commit_windows.go:61–161`).
- **VERIFIED:** Global config and global data have separate isolation overrides; `RUSH_GLOBAL_DIR` takes precedence over both (`internal/config/load_paths.go:16–26`, `:67–75`).
- **VERIFIED:** `sessions kill` does not blindly kill a stale PID when its OS-lock probe succeeds, and an unidentified probe error fails closed (`internal/cmd/sessions_kill.go:433–480`, `:508–513`).
- **VERIFIED:** Forced reset retains its acquired OS lock across the reset rather than unlinking the lock pathname (`internal/cmd/sessions_reset.go:87–101`).
- **VERIFIED:** Reap dry-run now skips acquisition; the Windows removal pass also performs a second probe, so the earlier reports were not repeated unchanged (`internal/cmd/sessions_reap.go:163–166`, `:259–273`).
- **VERIFIED:** CLI forking delegates to a transaction covering source reads, destination creation, and message copies, with rollback on failure (`internal/cmd/sessions_fork.go:107–117`; `internal/session/session_fork.go:60–215`).
- **VERIFIED:** Heartbeat process identity treats an unknown probe conservatively and detects a mismatched creation token before retaining a snapshot as live (`internal/heartbeat/writer.go:179–196`).

#### FILES READ

(83 entries not reproduced here)

### Orchestrator verification

- P0-1 (default `sessions gc` deletes actively resumed sessions): **CONFIRMED, rated P1** (explicit maintenance command; narrow selector). `classifyForGC` selects any session whose id starts with `ping-` and is older than one hour, and any orphaned child older than 24 h, regardless of message count, held session lock or live driver (sessions_gc.go:238-250); the only later protection is `gcDropBatchProtected` (batch roots and delivery debt), and the loop calls `Sessions.Delete` directly (:215). A user-named `ping-*` session that is resumed after an hour, or an active old orphaned child, is deleted with its transcript while it runs. Task created (maintenance commands need one shared live-ownership gate).
- P0-2 (plain `sessions reset` wipes a streaming web turn): **CONFIRMED, rated P1.** Only the `--force` branch takes the session lock (sessions_reset.go:78-102); the non-force path calls `resetLiveLoopRefusal`, which refuses only when `Facts.Driver != nil` (:104-113 region), then the batch reset that refuses only nonterminal batch roots. A web turn has neither a CLI driver nor a batch root, so the reset passes every gate and deletes the in-progress assistant message, contradicting the command's own help text ("a session that is still worked on is never reset"). Fix: take the session lock for the non-force path too and refuse when it is held.
- P1-3 (a surviving config writer invalidates an interrupted migration's recovery record): **NOT independently re-verified** (ktav_migration.go and store_write.go not read by the orchestrator). Requires a crash of the migrating process between publishing Ktav and retiring the record, and a second live writer that loaded the legacy slot. Cross-process crash timing: reproduction-first, otherwise P2.
- Backlog (P2/P3): `providers refresh` swallows persistence errors and `--all` exits 0 when every refresh fails; `providers add --local --enable` writes global scope; provider ids with dots are concatenated into sjson paths unescaped; `sessions diff` hand-scans JSON strings without unescaping; `sessions tree` loses indentation for top-level roots; claude-init/claude-del legacy-block removal rewrites the user's file with a truncating `os.WriteFile` (non-atomic).

---

## W3-05 — Test-suite health (wall-clock flakes)

Reviewer session `rv3-05-test-suite-health`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
**VERIFIED by static inspection:** three concrete test-harness failure scenarios remain, including two ways a slow runner can turn a bounded timeout into a package hang. Other timing-sensitive assertions are ranked below as P2 backlog items; their actual CI failure frequency is **INFERRED**, not measured. No files changed, and no builds, tests, linters, Go commands or Rush binaries were run.

#### P0/P1 findings

##### [P1] MCP timeout failures deadlock cleanup before releasing the fixture’s close barrier
- Where: `internal/agent/tools/mcp/lifecycle_perf_regression_test.go:78`, `internal/agent/tools/mcp/lifecycle_perf_regression_test.go:101`, `internal/agent/tools/mcp/lease_followup_test.go:56`, `internal/agent/tools/mcp/operation_lease_test.go:228`; `internal/agent/tools/mcp/owner.go:487`, `internal/agent/tools/mcp/owner.go:553`
- Scenario: Run `TestRetirementReleaseQueuesExactlyOneBlockingClose` on a contended Windows runner. After launching the final-operation-release goroutine, let that goroutine remain unscheduled for over one second while the test goroutine resumes. The timeout at line 101 calls `t.Fatal`, skipping `close(releaseClose)` at line 112. The deferred owner shutdown then never returns.
- Mechanism: The fixture’s `terminal` callback blocks on `releaseClose` (`lifecycle_perf_regression_test.go:82–85`). Its only release is on the success path. `Owner.Close(context.Background())` waits without a deadline, and owner shutdown waits for tracked sessions to finish (`owner.go:496–500`, `owner.go:553`). Session close cannot finish until that callback returns (`internal/agent/tools/mcp/session.go:70–88`). The same missing failure-path barrier release occurs in **four tests**: the two retirement-release tests in `lifecycle_perf_regression_test.go:75,122`, `lease_followup_test.go:53`, and `operation_lease_test.go:225`.
- Fix idea: Register an idempotent barrier release immediately after creating each gate, ordered to run before owner shutdown. Also release outstanding operation references on failure paths.
- Confidence: high; **VERIFIED** control flow. The scheduler delay is the specified trigger, not an observed CI reproduction.

##### [P1] Process fixture spends its execution deadline building its helper
- Where: `internal/jobbatch/lab/process_test.go:43`, `internal/jobbatch/lab/process_test.go:71`, `internal/jobbatch/lab/process_test.go:90`; `internal/jobbatch/lab/process.go:59`
- Scenario: On the first process test in a cold or disk-contended Windows shard, the helper build takes 12 seconds but succeeds. The subsequent executor start immediately fails with `context.DeadlineExceeded`, although no process-executor operation exceeded its allotted time.
- Mechanism: In `executor.Start(processTestContext(t), processTestLaunch(processTestHelper(t), ...), ...)`, the ten-second context is created before the helper-building argument is evaluated (`process_test.go:90`). The helper build itself permits up to one minute (`process_test.go:43–46`). A successful build lasting between ten and sixty seconds therefore passes an already-expired context into `Start`, which rejects it immediately (`process.go:59–61`). `sync.Once` makes this an invocation-order-sensitive failure affecting whichever test first requests the helper.
- Fix idea: Resolve/build the helper into a local variable before creating the executor-start context. Keep compilation and process-operation watchdogs separate.
- Confidence: high; **VERIFIED** evaluation order and deadline checks. Actual helper-build duration on CI was not measured.

##### [P1] Await-tasks test joins a helper that ignores the run’s cancellation
- Where: `internal/app/app_run_await_tasks_test.go:102`, `internal/app/app_run_await_tasks_test.go:110`, `internal/app/app_run_await_tasks_test.go:135`; `internal/app/app_run_loop_test.go:104`
- Scenario: In `TestRunLoop_AwaitTasksAny_WakesOnFirstCompletion`, the first provider request completes and its helper completes delegation 1. Before provider request 2 starts, CPU/disk contention delays the continuation until the sixty-second run context expires. The run returns without setting `req2At`; the test then hangs in `completers.Wait()` instead of reporting the deadline failure.
- Mechanism: The helper polls `req2At` indefinitely and has no context/stop branch (`app_run_await_tasks_test.go:110–112`). Only provider request 2 sets it (`:117`). The run has a sixty-second deadline (`app_run_loop_test.go:106`), but the unconditional join happens before checking `runErr` (`app_run_await_tasks_test.go:131–137`). Cancellation cannot terminate the polling helper.
- Fix idea: Replace the polling loop with a request-start channel selected against the run context or a test-owned stop channel. Ensure cancellation releases the helper before joining it.
- Confidence: high; **VERIFIED** cancellation gap and blocking join. The described runner delay was not exercised.

#### Backlog (P2/P3)
- [P2] `internal/agent/tools/mcp/state_regression_refresh_test.go:194,226` — **two lifetime tests** set a one-second admission timeout despite doing a real HTTP handshake or launching a subprocess; slow startup can fail before the intended “session survives admission expiry” assertion, and Ping plus CallTool share a further two-second budget at `:210–214,243–247` (VERIFIED).
- [P2] `internal/agent/tools/mcp/transactional_update_test.go:453,463` — **two assertions** sleep only 50 ms after asynchronous MCP notifications before requiring cleared tools; delayed notification handling produces a false failure rather than waiting for the actual transition (VERIFIED).
- [P2] `internal/server/handlers_livework_test.go:317` — the coalescing test allows 650 ms sleep plus a 50 ms collection window for a background worker that performs database snapshot reads before broadcasting (`internal/server/handlers_livework.go:286–306`); slow reads can leave the asserted snapshot list empty (VERIFIED).
- [P2] `internal/agent/tools/mcp/renewal_followup_test.go:97,156,177` — **three startup/publication waits** allow only two seconds around real replacement MCP initialization; these bypass the package’s existing 15-second await helper (`zz_commit_outcome_test.go:127–158`) (VERIFIED).
- [P2] `internal/app/app_run_await_tasks_test.go:277` — delegation completion starts after a 300 ms sleep, not after `await_tasks` creates its schedule; if tool processing is slower, the task finishes before the live-work capture and the expected cancelled schedule need never exist (`internal/agent/coordinator_await_tasks.go:90–108`); the adjacent “any” regression already uses an after-turn barrier (`app_run_await_tasks_maxwait_regression_test.go:37`) (VERIFIED).
- [P2] `internal/cmd/sessions_watch_configured_datadir_test.go:95` — cancellation begins before the command’s second `setupApp`, session resolution and initial reads (`internal/cmd/sessions_watch.go:69–130`); setup taking over two seconds can produce a cancellation error before any lock-location assertion is exercised (VERIFIED).
- [P2] `internal/agent/agent_title_trigger_test.go:288,352` — **two whole-turn latency assertions** demand completion within three/five seconds, including HTTP and persistence work; they do not isolate title-join time from unrelated slow-runner work (VERIFIED).
- [P2] `internal/agent/tools/mcp/state_regression_test.go:368`, `state_regression_mutation_test.go:748`, `state_regression_refresh_test.go:79` — **three enqueue/callback assertions** use 100/100/500 ms wall-clock thresholds; scheduler preemption alone can violate them without synchronous blocking (VERIFIED).
- [P2] `internal/session/p1_2_regression_test.go:325` — the hung-cleanup test requires Release within 100 ms despite real handle operations and bounded waits (`internal/session/lock.go:648–656,722–723,771–774`); it also intentionally leaks the cleanup goroutine, and its claim that the goroutine is GC’d when the test completes at `:351–352` is false (VERIFIED).
- [P2] `internal/server/dispatch_test.go:34,64,102,149,212,258` — **six standalone clients** start twelve permanent work-queue workers each (`internal/server/hub.go:36,130–155`), but these tests never close their queues; 72 idle workers accumulate per invocation, more under repeated runs (VERIFIED).
- [P2] `internal/server/dispatch_test.go:130–135` — concurrency is sampled after 300 ms and only checked for an upper bound; a delayed burst can be released before saturating the pool, so the test can miss an excessive-concurrency implementation (VERIFIED).
- [P2] `internal/agent/tools/job_test.go:298,320` — **two process-state assertions** infer completion/non-completion from five-second sleeps; slow shell startup or a test-goroutine pause past the twenty-second child sleep invalidates those assumptions (VERIFIED).
- [P2] `internal/jobbatch/runner_commit_projection_test.go:305,345–351` — a single five-second context covers acceptance of 450 callers plus intervening sampling; callers not accepted before expiry can abandon requests (`internal/jobbatch/runner.go:450–461`), leaving the test waiting for acceptance tokens that cannot arrive (VERIFIED).
- [P2] `internal/cmd/sessions_watch_test.go:404` — **two checked examples** of local-process coverage are skipped by Windows CI’s unconditional `-short`: the aged-live-lock watch test and live-holder probe/kill test (`sessions_kill_test.go:163`); the explicit no-short supplemental lane is Linux-only (`.github/workflows/ci-tests.yml:195–198`, `.github/workflows/ci-windows-shards.yml:231`) (VERIFIED).

#### Checked and found sound
- **VERIFIED:** The #310 candidate-close watcher now waits for request cancellation or test cleanup, rather than abandoning observation after five seconds: `internal/agent/tools/mcp/admission_retry_regression_test.go:91–105`.
- **VERIFIED:** Testguard cleanup preserves its isolation marker through process exit: `internal/testguard/testguard.go:59–76`; app teardown also quiesces the heartbeat writer before deleting isolated paths: `internal/app/main_test.go:23–27`.
- **VERIFIED:** Both platform-specific MCP test entry points install testguard isolation: `internal/agent/tools/mcp/main_test.go:11`, `internal/agent/tools/mcp/process_windows_test.go:36–52`.
- **VERIFIED:** The repaired live-attach test waits for the retry’s joined signal before releasing the provider, and checks returned errors before comparing responses: `internal/agent/batch_live_attach_identity_test.go:115–120,144–158`.
- **VERIFIED:** Several short watchdog sleeps are virtual, not wall-clock correctness windows; the inspected bump, tool-pause and sequential-progress tests run inside `synctest.Test`: `internal/agent/stream_watchdog_test.go:19`, `internal/agent/stream_watchdog_toolpause_test.go:23,96,134,192`.
- **VERIFIED:** The session generation-clobber test gates stale cleanup until the replacement generation is written, rather than assuming acquisition finishes within a sleep: `internal/session/p1_5_clobber_fix_test.go:59–64,94–106`.
- **VERIFIED:** The batch-path child-midturn test uses explicit command/provider gates rather than the legacy six-second sleep: `internal/app/app_run_subagent_async_midturn_test.go:84–108,147–163`.

#### FILES READ

(86 entries not reproduced here)

### Orchestrator verification

- P1-2 (process fixture spends its execution deadline building its helper): **CONFIRMED.** `executor.Start(processTestContext(t), processTestLaunch(processTestHelper(t), args, cap), ...)` (internal/jobbatch/lab/process_test.go:83): Go evaluates the arguments left to right, so the 10-second context (:64-69) is created BEFORE `processTestHelper(t)` runs `go build` of the helper under `sync.Once` with a one-minute allowance (:42-50). A build that takes 10-60 s on a cold or contended Windows shard hands `Start` an already expired context. Whichever test first asks for the helper fails with `context.DeadlineExceeded` although no process operation was slow. Real flake mechanism for the Windows shards.
- P1-3 (await_tasks test joins a helper that ignores the run's cancellation): **CONFIRMED.** The completer goroutine polls `for req2At.Load() == 0 { time.Sleep(20ms) }` with no stop condition (internal/app/app_run_await_tasks_test.go:109-111), and the test calls `completers.Wait()` before looking at `runErr` (:131-137). When the run ends without provider request 2 the test hangs until the go test timeout instead of reporting the failure. It only matters on the failure path (it turns a failure into a hang), so it is a diagnosability defect rather than a flake source of its own.
- P1-1 (MCP timeout failures deadlock cleanup before releasing the fixture's close barrier): **NOT independently re-verified** (lifecycle_perf_regression_test.go and the three other tests were not read by the orchestrator). Same class as P1-3: only reachable after a test has already failed, and then it hangs instead of failing.
- All three are test-harness defects; per the repository rule they are fixed in place (one batch), not left as backlog. The long P2 list (fixed one- to five-second windows around real HTTP/subprocess work in the mcp tests, 50-650 ms sleeps after asynchronous work, whole-turn latency assertions, 72 leaked idle workers in dispatch tests, `-short` hiding two local-process tests on Windows, a false "GC'd" comment) stays in the backlog as a planned test-hardening package, grouped by pattern with the counts the reviewer gave.

---

## W3-13 — Architecture and maintainability

Reviewer session `rv3-13-architecture`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict
**VERIFIED:** The inspected architecture has useful boundaries, but ownership and test-fixture compatibility still cross them: the coordinator exposes 34 methods, config imports provider execution packages, and batch bindings remain process-global (`internal/agent/coordinator.go:134–256`, `internal/config/load_providers.go:19–20`, `internal/agent/batch_install_ingress.go:144–149`). **No new P0/P1 finding. INFERRED:** The three highest-value defect-rate reductions are explicit instance-owned batch lifetimes, typed coordinator capability wiring, and test fixtures that exercise production implementations without production-duration waits.

#### P0/P1 findings
None established by this read-only architecture review.

#### Backlog (P2/P3)
- [P2] `internal/agent/batch_install_ingress.go:144–191` — **Make coordinator batch bindings instance-owned and explicitly releasable.** The global map stores each coordinator and its binding; repository reference search found deletion only in tests, while the process binding is first-wins and never cleared in production. **Cost:** medium; change binding storage and lifecycle wiring without moving packages. **Risk if deferred:** repeated SDK App creation retains coordinator/runtime object graphs indefinitely, and legacy fallback lifetime remains disconnected from App lifetime. **VERIFIED** retention mechanism; **INFERRED** operational impact.
- [P2] `internal/agent/coordinator.go:134–256`, `internal/agent/coordinator.go:258–443`, `internal/app/app_agent_setup.go:152–225` — **Replace implicit coordinator capability discovery with typed construction-time bundles.** The public interface has **34 methods**, yet App setup still installs execution requirements, disk capabilities, batch controls, driver claims, cutover facts and scheduling through separate operations or optional assertions; the scheduler is explicitly optional to preserve test fakes. **Cost:** medium–high; introduce focused capability bundles incrementally within existing packages, not a broad package split. **Risk if deferred:** necessary wiring can compile while silently disappearing; existing compile-time guards cover three optional interfaces, not the whole initialization contract (`internal/agent/coordinator_iface_assert.go:3–14`). **VERIFIED** structure; **INFERRED** future failure risk.
- [P2] `internal/config/load_providers.go:19–20`, `internal/config/load_providers.go:401–430`, `internal/agent/cliprovider/provider.go:16–20` — **Separate provider catalog/discovery metadata from provider execution dependencies.** Config imports `cliprovider` to obtain availability and catalog fields, but that package also imports permission and session services; Hyper similarly mixes embedded catalog/environment helpers with its Fantasy implementation (`internal/agent/hyper/provider.go:24–68`). **Cost:** medium; extract narrowly scoped metadata/discovery contracts, preserving current provider behavior. **Risk if deferred:** low-level configuration stays coupled to execution/session changes, broadening rebuild and test setup dependencies and constraining future import direction. **VERIFIED** dependency edges; **INFERRED** maintenance impact.
- [P2] `internal/agent/coordinator.go:452–456`, `internal/agent/coordinator_model_cache.go:99–163`, `internal/agent/coordinator_models_build.go:262–266` — **Retire production cache compatibility branches maintained solely for old test fixtures.** Production constructs the bounded cache (`internal/agent/coordinator.go:509`), while the **65-line legacy compatibility region** serializes keys and dynamically discovers `Get`/`Set`; clearing separately discovers `Reset`. Tests still construct `csync.Map` caches (`internal/agent/coordinator_test.go:150–163`). **Cost:** low–medium; migrate fixtures, then use the concrete production cache or a complete behavioral interface. **Risk if deferred:** many tests exercise different caching behavior, and the declared interface guarantees only `Len`, not the operations the implementation actually needs. **VERIFIED**.
- [P2] `internal/app/app_run_quota_child_test.go:28–65`, `internal/app/app_run_timeout_test.go:46–52`, `internal/agent/agent_turn_stream.go:235–237` — **Make retry/wait policy instance-injectable for integration tests, retaining separate production-policy tests.** Quota tests calculate real dependency backoffs and keep child work alive beyond them; timeout tests explicitly budget those waits. Other integration fixtures already bypass waits by mutating process-global retry variables (`internal/app/app_run_continuation_final_text_test.go:119–129`). The historical baseline identifies App/config/cmd/agent as **78% of 2,470 seconds**, but is not a current timing measurement (`docs/ci-speed.md:3–5`, `docs/ci-speed.md:21–46`). **Cost:** medium; add scoped policy injection and migrate the expensive fixtures without dropping assertions or exhaustive coverage. **Risk if deferred:** dependency retry changes inflate CI and encourage unsafe global test overrides. **VERIFIED** wait construction; **INFERRED** current aggregate cost.
- [P2] `README.md:145–157`, `internal/server/handlers.go:170–181`, `internal/server/handlers_agent.go:199–201` — **Correct the public web-permission contract.** README says web mode still displays per-tool permission dialogs; the actual web send path unconditionally calls `AutoApproveSession`, and its implementation explicitly states there is no dialog. **Cost:** low; align the security section and related feature descriptions with current behavior, without changing policy. **Risk if deferred:** operators may select web mode expecting a human approval boundary that does not exist. **VERIFIED** documentation contradiction; no independent policy-bypass claim.
- [P2] `docs/async-invariants.md:39–48`, `docs/async-invariants.md:54–59`, `internal/agent/coordinator.go:463–467` — **Re-anchor or explicitly retire the pre-batch invariant registry, then validate executable anchors automatically.** The registry describes removed `async_job_store.go`/`work_ledger_*` implementations as current and requires same-commit re-anchoring; filename searches found neither family, while current coordinator documentation identifies batch-only execution. **Cost:** medium; map retained laws to batch code/tests and mark genuinely historical material. **Risk if deferred:** reviewers use nonexistent enforcement points and obsolete test names as architectural evidence. **VERIFIED** stale anchors and replacement architecture; individual invariant correctness was not re-audited.

#### Checked and found sound
- **VERIFIED:** The inspected dependency seams preserve `cmd → app → agent` and `agent → session`; the run-queue interface lives in its consuming package, with an App adapter instead of a session import of agent (`internal/cmd/root.go:28–35`, `internal/app/app.go:14–25`, `internal/session/run_queue_pump.go:77–88`, `internal/app/app.go:29–30`).
- **VERIFIED:** The sampled batch core keeps deterministic state transitions separate from physical execution and persistence publication: Engine applies events, Executor owns effects, and Runner’s committer returns acknowledgements to the owner (`internal/jobbatch/engine.go:9–28`, `internal/jobbatch/executor.go:24–38`, `internal/jobbatch/runner_commit.go:48–65`). This is not a proof of every runtime interleaving.
- **VERIFIED:** The size allowlist contains **zero exemption entries**, and the guard distinguishes generated files by content (`.githooks/file_size_allowlist.txt:1–11`, `.githooks/check_file_size.sh:119–146`). Inspected near-limit handwritten files end at **950 lines** and **975 lines**, respectively (`internal/agent/agent.go:950`, `internal/config/load_providers.go:975`); further mechanical splitting alone would not resolve their ownership coupling.
- **VERIFIED:** SQL generation has a clear source/output boundary: queries and migrations are sqlc inputs, and generated DB code carries its marker (`sqlc.yaml:3–14`, `internal/db/db.go:1–3`).
- **VERIFIED:** Model resolution captures configuration and generation together; the production model cache is bounded and fences publication against an invalidation epoch (`internal/agent/coordinator_models.go:112–114`, `internal/agent/coordinator_model_cache.go:65–84`, `internal/agent/coordinator_model_cache.go:147–156`).
- **VERIFIED:** Read-only review completed with **no files changed** and **no builds, tests, linters, Go commands or Rush binaries run**. No dependency-cache source was consulted.

#### FILES READ

(61 entries not reproduced here)

### Orchestrator verification

- No P0/P1, as the reviewer says. Seven structural P2 recommendations (instance-owned batch bindings; typed coordinator capability bundles; separating provider catalog metadata from execution dependencies; retiring the cache compatibility branches kept for old fixtures; injectable retry/wait policy for integration tests; correcting the README web-permission text; re-anchoring `docs/async-invariants.md`) go to the backlog as planned work, not into this cycle.
- README web-permission contradiction: **CONFIRMED.** README.md:154-157 says the web UI "still shows a permission dialog (Allow / Deny / Allow-always) for each tool call", while the send handler re-arms session auto-approval on every human message (`autoApproveWebSession(a, p.SessionID)`, internal/server/handlers_agent.go:199-201; the reviewer cites `AutoApproveSession` stating that there is no dialog). A reader choosing web mode for its approval boundary is told something the code does not do. Documentation fix only; whether the web mode SHOULD gate tools is a product decision for the operator, not a review finding.
- A citation could not be reproduced: the reviewer's baseline reference `docs/ci-speed.md:3-5` does not exist in the checked-out tree (`ls docs/ci-speed.md` fails), so the "78% of 2,470 seconds" figure is unverified.
- The size-limit observation (allowlist empty, largest inspected hand-written files at 950 and 975 lines) was not re-measured by the orchestrator; 975 is close enough to the 1000-line rule to be worth a glance at the next change to `internal/config/load_providers.go`.

---

## W3-07 — MCP subsystem against docs/mcp-invariants.md

Reviewer session `rv3-07-mcp-invariants`; read-only, detached checkout of origin/main 2eb4c371.

### Reviewer report (verbatim; the FILES READ list is reduced to a count)

#### Verdict

The MCP lifecycle guards remain substantial and have meaningful regression oracles, but I found **one P0 and three P1 issues**: credential forwarding across HTTP redirects, a leaked enable admission that permanently blocks owner shutdown, an unbounded post-connect discovery phase, and stale cross-store definitions accepted after field persistence. These are source-reviewed findings, not executed reproductions. No files were changed; no builds, tests, linters, Go commands, or Rush binaries were run. Registry anchors have drifted, and several stated guarantees exceed what the current code or tests establish.

#### P0/P1 findings

##### [P0] Redirected MCP requests receive the original endpoint’s credentials
- Where: `internal/agent/tools/mcp/transport.go:136`, `internal/agent/tools/mcp/transport.go:163`, `internal/agent/tools/mcp/transport.go:237`
- Scenario: Configure an HTTP/SSE MCP endpoint with `headers.Authorization = "Bearer …"`. That endpoint returns a redirect to a different host—for example, a compromised endpoint redirects initialization to an attacker-controlled HTTPS collector. The redirected request receives the configured bearer token.
- Mechanism: Both transport constructors use an `http.Client` without a redirect restriction. `headerRoundTripper.RoundTrip` unconditionally installs every configured header on **every** request at lines 241–242, including redirected requests. Thus even headers removed by the HTTP client’s cross-host redirect handling are reintroduced immediately before transmission.
- Fix idea: Reject cross-origin redirects, or bind header injection to the configured endpoint’s origin and explicitly prevent credential forwarding on origin changes.
- Confidence: high; **VERIFIED** injection mechanism and client configuration. The redirect scenario was not executed.

##### [P1] Cancellation after enable admission permanently strands the owner
- Where: `internal/agent/tools/mcp/mutation.go:461`, `internal/agent/tools/mcp/mutation.go:479`, `internal/agent/tools/mcp/admission.go:310`, `internal/agent/tools/mcp/admission.go:341`, `internal/agent/tools/mcp/owner.go:512`
- Scenario: Enable a disabled server with a cancellable context. Let persistence succeed, then cancel the context immediately before admission creation or between admission creation and the validity check. `EnableServer` returns an error, but subsequent `Owner.Close(context.Background())` never returns; acquiring a replacement installed owner remains blocked.
- Mechanism: Admission creation derives an already-cancelled child context but still registers its cancellation token and increments `initWG`. The check at `mutation.go:479` rejects it. That return branch performs rollback and retirement but **never calls `admission.done()`**. The initializer—the normal owner of that cleanup—is never launched. Consequently the admission’s wait-group reference survives forever, and shutdown waits at `owner.go:512`.
- Fix idea: Complete the admission on every return after successful admission creation, transferring cleanup ownership explicitly only when the initializer is launched.
- Confidence: high; **VERIFIED** reference leak and shutdown dependency. The scheduling scenario was not executed.

##### [P1] Discovery after successful connection has no initialization timeout
- Where: `internal/agent/tools/mcp/session.go:765`, `internal/agent/tools/mcp/publish.go:20`, `internal/agent/tools/mcp/tools.go:351`, `internal/agent/tools/mcp/prompts.go:206`, `internal/agent/tools/mcp/init.go:251`
- Scenario: An enabled server completes MCP initialization promptly, accepts `tools/list`, and then leaves that request open indefinitely without returning a result. Invoke full initialization with a context having no deadline. Initialization never finishes, and a run waiting for its initialization barrier cannot proceed. The same problem applies to a server that stalls `prompts/list` after returning its tools.
- Mechanism: `createSessionWithAdmission` stops its initialization timer immediately after `Connect`. `prepareClient` subsequently calls `getTools` and `getPrompts` with the original operation context, without another deadline. Full initialization waits for that preparation at `init.go:251`. The transport’s HTTP client also has no overall request timeout (`transport.go:136`).
- Fix idea: Give the entire unpublished preparation phase—connection plus discovery—a bounded candidate context. Disarm that bound only when preparation/publication succeeds, without imposing it on the published session.
- Confidence: medium; **INFERRED** end-to-end hang from the verified local timeout gap. The pinned MCP SDK v1.8.0 was unavailable locally, so SDK-internal timeout behavior was not independently checked.

##### [P1] Field persistence imports fresh input tokens while retaining another server’s stale definition
- Where: `internal/config/store_mcp.go:276`, `internal/config/store_mcp.go:331`, `internal/config/store_mcp_transaction.go:800`, `internal/config/store_mcp_admission.go:258`, `internal/agent/tools/mcp/publish.go:96`
- Scenario:
  1. Store S loads servers A and B; B points to endpoint B-old.
  2. Another store changes B on disk to B-new.
  3. Without reloading S, S successfully calls `PersistMCPFieldsExact` to change a field of A.
  4. S initializes B.
  
  B can connect to and publish **B-old**, although its admission passes validation against disk containing B-new.
- Mechanism: The field transaction evaluates all current disk definitions and captures all their `mcpInputs`. Publication changes only A in the retained config map (`store_mcp.go:342`) but replaces the **entire** input-token map (`:348`). B therefore keeps its stale configuration paired with B-new’s source token. Admission compares the token against fresh disk (`store_mcp_admission.go:266`) and compares the definition only against S’s own retained snapshot (`:633`); it does not compare the candidate definition with `evaluation.configs[B]`. The ordinary mutation publisher has the same whole-token-map/selected-definition mismatch at `store_mcp_transaction.go:811–830`.
- Fix idea: Publish effective definitions and their source tokens from the same evaluation. Alternatively, update only tokens corresponding to definitions actually reconciled into memory, leaving other changes detectably stale.
- Confidence: high; **VERIFIED** inconsistent snapshot construction and admission checks. The multi-store scenario was not executed.

#### Backlog (P2/P3)

- [P2] `docs/mcp-invariants.md:31` — INV-06’s blanket “no disk I/O under lifecycleMu or a server lease” contradicts intentional lease-held persistence (`addreplace.go:352`) and final filesystem validation under `lifecycleMu` (`admission.go:616`, `internal/config/store_mcp_admission.go:108`); specify the actual exceptions (VERIFIED).
- [P2] `internal/agent/tools/mcp/refresh.go:54` — one stalled list request occupies the owner’s sole refresh worker and prevents refreshes for unrelated servers; refresh contexts have no per-request deadline (`refresh.go:225`) (VERIFIED local serialization; INFERRED indefinite stall).
- [P2] `internal/agent/tools/mcp/tools.go:355` — tool discovery consumes one list result; prompts and resources do likewise (`prompts.go:210`, `resources.go:261`), with no cursor handling in this package; pagination completeness needs checking against pinned SDK v1.8.0 (INFERRED).
- [P2] `internal/agent/tools/mcp/process_other.go:28` — Windows cancellation relies on PID-tree killing without registering a tracked job; an exited intermediary/root can leave descendants outside that mechanism’s reach (`internal/session/kill_windows.go:51`) (INFERRED).
- [P2] `internal/agent/tools/mcp/mutation.go:488` — asynchronous connection failure does not run enable rollback; INV-13’s “failed Enable restores or fences” wording is broader than the implemented pre-launch rollback contract (`docs/mcp-invariants.md:38`) (VERIFIED).
- [P2] `internal/agent/tools/mcp/init.go:47` — the barrier opens inside `Initialize`, while application startup launches it asynchronously (`internal/app/app.go:513`); an earlier `WaitForInit` observes the initially closed channel and can return before startup begins (VERIFIED).
- [P2] `internal/agent/tools/mcp/owner.go:836` — detachment deletes committed admission bookkeeping only from the installed owner, leaving standalone-owner entries referencing detached sessions until replacement/close (VERIFIED).
- [P3] `docs/mcp-invariants.md:26` — registry line anchors and several test-file locations predate later splits; notably owner lifecycle tests moved to `init_lifecycle_test.go`, and mutation/refresh tests moved out of `state_regression_test.go` (VERIFIED).
- [P3] `internal/agent/tools/mcp/state_regression_refresh_test.go:532` — the named rollover-barrier test never starts full initialization, so its old-owner waiter observes an already-closed barrier rather than exercising release of an open one (VERIFIED).
- [P2] `internal/agent/tools/mcp/ktav_testgate_test.go:31` — almost all lifecycle oracles run with shipped-default Ktav migration disabled; the explicit opt-in test checks persistence selection, not lifecycle/admission round trips (VERIFIED).

#### Checked and found sound

**Registry verification:** All 25 rows still have identifiable enforcing code and at least one named, non-tautological test. This does **not** mean every clause is satisfied: exceptions and uncovered violations are identified below. Test locations are current; all paths in this table are under `internal/agent/tools/mcp/`.

| Invariant | Current enforcement inspected | Named oracle inspected / result |
|---|---|---|
| INV-01 | `session.go:613`, `session.go:687`, `publish.go:200` | `init_lifecycle_test.go:294` checks both cancellation winners and promotion; published-lifetime behavior is asserted at `state_regression_refresh_test.go:174`. **VERIFIED** handoff remains present. |
| INV-02 | `owner.go:334`, `owner.go:416`, `owner.go:581` | `uncertainty_fence_test.go:166` rejects another store; `standalone_owner_test.go:197` checks surviving live tools across close orders. **VERIFIED** for the documented disjoint-name contract. |
| INV-03 | `admission.go:184`, `admission.go:233`, `mutation.go:175` | `state_regression_test.go:190` injects failed persistence and subsequently requires candidate publication. **VERIFIED** failed disable/remove do not invalidate candidates. |
| INV-04 | `owner.go:504`, `owner.go:545` | `init_lifecycle_test.go:630` requires timeout fencing and eventual reacquisition. **VERIFIED** join/fence mechanism; leaked admission finding above violates its completion assumptions. |
| INV-05 | `owner.go:828`, `session.go:403`, `publish.go:228` | `committed_admissions_sync_test.go:82` exercises cross-name detach/write/read and checks final contents. **VERIFIED** lifecycle locking remains. |
| INV-06 | `lease.go:281`, `addreplace.go:352` | `lock_discipline_test.go:86` and `:114` probe real seams. **VERIFIED** narrow checks; blanket registry wording is inaccurate. |
| INV-07 | `lease.go:111`, `lease.go:125`, `lease.go:281` | `state_regression_refresh_test.go:108` checks retained identity/reclamation; `lock_discipline_test.go:37` checks reversed-name sorting. **VERIFIED**. |
| INV-08 | `addreplace.go:740`, `publish.go:244`, `mutation.go:701` | `zz_commit_outcome_test.go:588` uses acquisition hooks and asserts Connected precedes Deleted. **VERIFIED** checked add/remove ordering. |
| INV-09 | `addreplace.go:305`, `addreplace.go:330`, `addreplace.go:401` | `transactional_update_test.go:66` checks old runtime/disk preservation; `:107` checks callbacks after persistence failure. **VERIFIED** checked failure paths. |
| INV-10 | `admission.go:122`, `admission.go:159`, `addreplace.go:292` | `p1_p2_regression_test.go:16` raises a destination fence during preparation and requires no persist. **VERIFIED** dual-name fence. |
| INV-11 | `addreplace.go:647`, `addreplace.go:782`, `addreplace.go:842` | `uncertainty_fence_test.go:57` requires no config/session/state after precommit failure. **VERIFIED** staged publication/guarded rollback. |
| INV-12 | `mutation.go:175`, `mutation.go:669` | `state_regression_mutation_test.go:35` counts one conditional write and checks absence on disk/runtime. **VERIFIED**. |
| INV-13 | `mutation.go:388`, `mutation.go:442` | `p1_p2_regression_test.go:220` preserves a concurrent writer and requires a store fence. **VERIFIED** pre-launch CAS rollback; not asynchronous failure rollback. |
| INV-14 | `uncertainty.go:238`, `uncertainty.go:254` | `zz_commit_outcome_test.go:352` starts with a session, injects uncertainty, and requires detachment/fencing. **VERIFIED**. |
| INV-15 | `uncertainty.go:30`, `uncertainty.go:49` | `uncertainty_fence_test.go:118` requires the fence to survive owner replacement and clear only after reload. **VERIFIED**; store-side version comparison remains at `internal/config/mcp_uncertainty.go:66`. |
| INV-16 | `refresh.go:69`, `refresh.go:134`, `refresh.go:277` | `state_regression_test.go:380` requires exact-token pending work and no premature event/run; `:429` checks discard. **VERIFIED**. |
| INV-17 | `refresh.go:101`, `refresh.go:209` | `state_regression_mutation_test.go:698` asserts dirty rerun and latest tools; `state_regression_refresh_test.go:23` asserts saturation retention. **VERIFIED** coalescing. |
| INV-18 | `admission.go:798`, `admission.go:818` | `lifecycle_findings_add_test.go:677` requires Starting resolution, no session, and exactly one error event. **VERIFIED**. |
| INV-19 | `lease.go:185`, `lease.go:536`, `session.go:149` | `renewal_followup_test.go:16` checks winner/follower behavior; `operation_lease_test.go:111` checks deferred close. **VERIFIED** checked single-flight/pinning. |
| INV-20 | `transport.go:237`, `transport.go:210` | `state_regression_test.go:292` checks cancellation reaches the server; `init_lifecycle_test.go:556` checks open-body lifetime. **VERIFIED** cancellation fences. |
| INV-21 | `transport.go:109`, `process_unix.go:22`, `process_other.go:27` | `process_windows_test.go:99` checks a living grandchild dies after cancellation; `stdio_diagnostic_test.go:240` checks output limits. **VERIFIED** tested case, not every process-tree topology. |
| INV-22 | `init.go:47`, `init.go:81`, `init.go:273` | `state_regression_refresh_test.go:570` checks reopening; `admission_retry_regression_test.go:335` checks normal completion before wait-group completion. **VERIFIED** lazy/reopen behavior; startup scheduling gap remains. |
| INV-23 | `mutation.go:510`, `admission.go:412` | `origin_scope_regression_test.go:531` requires project mutations to fail closed without global writes. **VERIFIED** installed-owner path. |
| INV-24 | `admission.go:233`, `admission.go:268`, `lease.go:512` | `state_regression_mutation_test.go:468` requires live notifications after an unrelated write; `:507` checks relevant-change retirement. **VERIFIED** checked distinction. |
| INV-25 | `admission.go:600`, `admission.go:635` | `admission_final_source_linearization_test.go:16` checks same-size in-place changes plus unchanged control; `admission_retry_regression_test.go:778` checks cross-store retry and stale closure. **VERIFIED** guards remain, but the token/definition mismatch above bypasses their intended correspondence. |

- **VERIFIED:** Candidate-close paths inspected in preparation, publication rejection, replacement failure, and add rollback explicitly close or retire rejected sessions: `publish.go:37`, `publish.go:106`, `addreplace.go:316`, `addreplace.go:764`.
- **VERIFIED:** Retry counts are explicitly bounded, rather than using a timer to abandon stale-candidate cleanup: `admission.go:635`, `lease.go:394`. The #310 watcher now waits for cancellation or test cleanup: `admission_retry_regression_test.go:97`.
- **VERIFIED:** No package file extends beyond 1000 lines; the allowlist has no entries (`.githooks/file_size_allowlist.txt:1`). This was checked by read-only file inspection, not the hook.
- **Verification limitation:** `go.mod:39` pins MCP SDK **v1.8.0**. Only v1.6.1 was available in the inspected module cache; its source was deliberately not used.

#### FILES READ

(26 entries not reproduced here)

### Orchestrator verification

- P0-1 (redirected MCP requests receive the original endpoint's credentials): **CONFIRMED, rated P1.** Both HTTP and SSE transports build an `http.Client` with no `CheckRedirect` (transport.go:136-166), so Go follows up to ten redirects, and `headerRoundTripper.RoundTrip` sets every configured header on every request it sends (`for k, v := range rt.headers { req.Header.Set(k, v) }`, transport.go:237-242) — this re-adds `Authorization` that the client strips on a cross-host redirect. Rated P1 rather than P0 because the endpoint is operator-configured and already receives the token; the incremental exposure is the token reaching a different host through a redirect the operator did not intend (a CDN or storage redirect, an open redirect on the same host). Fix: refuse cross-origin redirects or bind header injection to the configured origin.
- P1-1 (cancellation after enable admission strands the owner): **NOT independently re-verified** (mutation.go:461-479, admission.go:310-341, owner.go:512 not read by the orchestrator). A reference-leak shape (`admission.done()` not called on a rejected-but-created admission) with a narrow cancellation window; the MCP subsystem rule applies: reproduction test first, narrow fix, no new guards.
- P1-2 (discovery after connect has no timeout): **NOT independently re-verified; INFERRED** by the reviewer (the pinned MCP SDK v1.8.0 was not available in its module cache, so SDK-internal timeouts were not checked).
- P1-3 (field persistence imports fresh input tokens while retaining another server's stale definition): **NOT independently re-verified** (store_mcp.go:276-348, store_mcp_admission.go:266/:633 not read). Multi-store scenario; reproduction first.
- Registry check: the reviewer states that all 25 rows still have identifiable enforcing code and at least one named non-tautological test, with INV-06's wording, INV-13's wording and many line anchors drifted (backlog items). The table was not re-audited row by row by the orchestrator.
- Backlog (P2/P3): INV-06 blanket wording contradicts lease-held persistence; one stalled list request blocks the sole refresh worker for all servers; list pagination cursors unhandled (INFERRED); Windows process-tree kill without a tracked job (INFERRED); asynchronous connect failure runs no enable rollback (INV-13 wording); `WaitForInit` can return before startup begins; detach leaves standalone-owner bookkeeping; registry anchors drifted; one named test never starts full initialization; almost all lifecycle oracles run with Ktav migration disabled.
