# Commit review — 7-day window, chunk 1 of 5 (`da53fe42a..7b928bb4b`)

> Reconciled 2026-09-09 against commit `4c2f11bd3` (main): the
> reconciliation closure list contained no C1 items, so every status
> below stands as written (items marked "closed at tip" remain closed).

43 commits, 2026-08-31 12:15 → 2026-09-02 09:14 CEST. This is the first
slice of the 215-commit window `da53fe42a..f2914d53c`, split by chain
position across five independent reviewers; chunks 2–5 are not covered
here.

Scope of the check: correctness, completeness, missed edge cases, whether
new/modified tests are non-vacuous, whether comments and commit messages
describe what the diff actually does, concurrency, goroutine/resource
leaks, error handling. Read-only mode: no `go build`, `go test`, `go vet`,
`gofmt`, lint, `pnpm`, `.githooks/*` or Makefile target was executed. No
existing file in the repository was modified; this document is the only
addition.

Thematically the range is the early SDK-embedding arc: `Client.Close`
idempotency, `RunWithCredentials` multi-tenant credential injection,
`sdk.Open` library mode, session/message `Origin`, per-call execution
context + atomic session admission (review findings R1-1…R1-8, R2-1…R2-3,
R3-1…R3-6, F1…F6, R4-1…R4-3), and the first six tasks of the scoped `fs_*`
tool family (T1–T6).

## Relationship to the existing review documents

Twelve commits in this range explicitly name a finding from a review
report. Those reports were located and read as review input:

| Report | Location | Findings |
|---|---|---|
| SDK/library review round 1 | `ad3bca8ba:docs/reviews/2026-08-31-sdk-library-review-round-1.md` (branch `research/upstream-gap-analysis-20260830`; not on `main`) | R1-1…R1-8 |
| round 2 | `31d8ed88f:docs/reviews/2026-08-31-sdk-library-review-round-2.md` (same branch) | R2-1…R2-6 |
| round 3 | `937df4566:docs/reviews/2026-09-01-sdk-library-review-round-3-1055.md` (same branch) | R3-1…R3-6 |
| round 4 | `eb94b67ed:docs/reviews/2026-09-01-sdk-library-review-round-4-2331.md` (same branch) | R4-1…R4-4 |
| independent re-read of the R3 arc | `docs/reviews/2026-09-01-sdk-review-fh.md` (on `main`) | F1…F6 |
| round 5 | `docs/reviews/2026-09-02-sdk-library-review-round-5-2331.md` (on `main`) | R5-1…R5-6 |

**What is cited vs. derived.** The per-commit dispositions for the R1/R2/R3
fix commits (`e900aea0a`, `4f88867a5`, `f4e3fd86a`, `cf0dfc64f`,
`9b3841e0c`, `692b28541`, `8474beb91`, `8341077dc`, `4f15041d3`,
`066dd6ad7`, `f24f17463`, `ddaccc1a6`, `271550b1f`) are **cited** from
rounds 3/4/5 and the fh re-read, with the specific mechanisms I re-derived
noted inline. Everything under "Findings" below (C1-1…C1-15) is **derived
fresh** in this pass unless it explicitly says otherwise; where a fresh
finding turned out to be the origin of a later-named finding (R5-1, R5-2,
R5-5, R6-2) I say so and mark whether it is still live at the worktree tip.

Note on rounds 1–4: those four reports are **not present on `main`**. They
exist only on the `research/upstream-gap-analysis-20260830` branch. R4-4
flagged exactly this class of problem (production comments referencing an
untracked review file) and was closed for `2026-09-01-sdk-review-fh.md`
by `f4e30ce4f`, but the R1/R2/R3 reports that `e900aea0a`, `cf0dfc64f`,
`9b3841e0c` and their siblings cite in their subject lines still cannot be
read from a clean `main` checkout. See C1-15.

---

## Commit summary

Chronological (oldest first).

| Hash | Theme | Verdict |
|---|---|---|
| `7d8571ed3` | githooks: fail closed when `git grep` errors | correct; self-test has no caller (C1-14) |
| `b6de58a6f` | config: worktree detection keyed on `WorkingDir` | correct; test is discriminating |
| `198c3d52f` | ci: batched `-race` for `internal/agent` + leak smoke | correct in intent; batch step fails open (C1-5) |
| `70db7fd80` | sdk: idempotent, status-returning `Close` | correct |
| `7dd4a7d23` | agent: resume command in timeout-stop messages | **wrong knob for `causeToolTimeout` (C1-3)** |
| `97ffd22ff` | sdk: `RunWithCredentials`, `readyGate`, tool-options wrapper | correct; two real pre-existing races genuinely fixed; C1-11 nit |
| `9e45f1824` | sdk: `Open` creates `WorkingDir` | correct |
| `c0d542986` | sdk: library mode, ephemeral in-memory sessions | correct at the time except goose global (C1-12, since closed) |
| `c335537f2` | sdk: fail fast on a busy session | correct; TOCTOU window documented, closed later by `cf0dfc64f` |
| `8f2f71043` | session/message `Origin` | correct; wiring is coherent end to end |
| `ae277c26b` | sdk: `Messages`/`Session` | correct |
| `887359735` | docs: SDK guide | correct for its moment; overtaken by R2-4/R3-5 |
| `e900aea0a` | R1-1/R1-4: per-call `CallOptions`, atomic admission | correct direction; left R2-1 open (cited) |
| `4f88867a5` | R1-2/3/5/6/7/8: unique mem DSN, fail-closed creds, API cleanup | correct; misleading error text (C1-10) |
| `9b9ec295a` | reword comment around a gofumpt rewrite | correct |
| `4269b799b` | widen release-gate test bound to 3s | correct |
| `70e38b3b9` | githooks: allow-list `shutdown_result_test.go` | correct; reasoning verified by hand |
| `591b649a6` | warm file cache before timing cancellation test | mitigates, does not remove the timing dependency (C1-13) |
| `f4e3fd86a` | R2-2: admission/shutdown state machine | correct; introduced the R3-2 ordering bug (cited), fixed by `692b28541` |
| `cf0dfc64f` | R2-1/R2-3: owner token before per-run mutations | correct (round 3 verified independently; re-checked here) |
| `c958425d1` | checkpoint doc | n/a |
| `6a1c5fbd5` | app: stop mapping `(nil,nil)` to `ErrSessionBusy` | **replaces a wrong error with a silent success (C1-1)** |
| `df6b2370c` | ci: split monolithic test step into named groups | correct; "pure regrouping" understates it (C1-9) |
| `9b3841e0c` | R3-1: pin per-call tool slice | correct core; **fail-open fallback (C1-2)**, later escalated to R5-1 |
| `692b28541` | R3-2: `Close` cancels admitted work | correct (round 4 + fh verified; spot-checked) |
| `746cf4dfb` | `PingContext` for noctx lint | correct |
| `8474beb91` | R3-3: observe cancellation during the admission hold | correct but incomplete; completed by `f24f17463` (F6) |
| `430c79a6e` | R3-5: shutdown/multi-client docs | correct; F4 found four per-method docs still stale |
| `8341077dc` | R3-4: policy versioned by `LogicalCallID` | correct in-process; durable gap → F2/R4-1 |
| `4f15041d3` | R3-6: explicit timeout presence | structurally correct, behaviourally inert until `f24f17463` (F3) |
| `ecea5a4c4` | checkpoint doc | n/a |
| `49612bfb9` | session: deterministic sync in p1-1 tests | correct |
| `752a811d3` | F1: pin `RunWithCredentials` tools | correct |
| `066dd6ad7` | F4/F5: reclaim guard on `closed`, per-method docs | correct |
| `f24f17463` | F3/F6: reachable `TimeoutOptionsSet`, gate every cleanup write | correct |
| `ddaccc1a6` | F2: per-session durable-restart baseline | **superseded**: introduced R4-1/R4-2 (P0), fixed by `271550b1f` |
| `271550b1f` | R4-1/2/3: persist + rebind policy by `LogicalCallID` | correct (round 5 confirms for new rows) |
| `232e62c42` | T1: `FolderScope` matcher | correct matcher; lexical roots → R5-2 (C1-6) |
| `4fc762ccf` | T2: `resolveScopedPath` | correct resolver; its own test encodes the R5-2 mismatch (C1-6) |
| `887fc9f63` | T3: `RunFSBatch` | correct; output budget is weaker than documented (C1-8) |
| `50e6dd687` | T4: `fs_read`/`fs_list`/`fs_find` | **`lines=` off-by-one (C1-4)**; lexical entry check → R6-2 |
| `3f007e6b3` | T5: `fs_grep` | correct merge/parse logic; **ignores `tools.grep.timeout` (C1-7)** |
| `7b928bb4b` | T6: `fs_write`/`fs_replace`/`fs_write_lines`/`fs_delete` | correct; `fs_write` snapshot gap → R5-5, fixed later |

Two commits (`c958425d1`, `ecea5a4c4`) are checkpoint documents only. Nine
commits have a substantive body; the rest of the range is unusually well
documented for this repository — most commit messages read as design notes
and, where I could check them against the diff, they were accurate. The
three exceptions are called out in C1-1, C1-3 and C1-9.

---

## 1. SDK lifecycle: `Close`, admission, shutdown (`70db7fd80`, `f4e3fd86a`, `692b28541`, `066dd6ad7`, `430c79a6e`, `746cf4dfb`)

`70db7fd80` splits `App.Shutdown` into `ShutdownWithResult` + a thin
wrapper and makes `sdk.Client.Close` idempotent via `sync.Once` with a
cached `CloseResult`. The mechanics are sound: the cleanup-error slice is
collected under `errMu` and snapshotted under the same mutex before
return, so an abandoned goroutine that appends past the 10s outer timeout
cannot be observed through the returned value
(`internal/app/app_lifecycle.go`, the `recordCleanupError`/snapshot pair
in the original diff). `sync.Once` gives the happens-before edge that
makes the cached `closeResult` safe for concurrent `Close` callers.

Two notes, neither a defect:

- `Close() error` became `Close() CloseResult`, so `*Client` no longer
  satisfies `io.Closer`. That is a deliberate, documented API break for a
  pre-1.0 package, but it is not mentioned in the commit message.
- `TestShutdownWithResult_CollectsCleanupErrors` constructs a bare `App`
  with nil `AgentCoordinator`, nil `RunQueuePump`, nil `globalCtx` and
  empty `dataDir`. That works only because `shutdownCtx` is derived from
  `context.Background()` rather than `app.globalCtx`; the test is
  therefore load-bearing on an implementation detail it does not state.

The `f4e3fd86a` → `692b28541` → `066dd6ad7` sequence is the R2-2 → R3-2 →
F4/F5 chain. I re-derived the R3-2 mechanism rather than taking it on
trust: at `f4e3fd86a`, `Client.Close` blocked on `<-drained` before calling
`app.ShutdownWithResult`, and `ShutdownWithResult`'s first action is
`AgentCoordinator.CancelAll()`. A stuck admitted `Run` therefore prevented
the only thing that could have cancelled it. `692b28541` inverts the order
via `ShutdownAfterDrain` (one drain window, then cancel while resources are
still live, then release), which is the right shape. `066dd6ad7` then moves
`CloseEphemeralConnsForced`'s guard from `closing` (set at the start of
phase 1) to `closed` (set after `ShutdownAfterDrain` returns); I confirmed
that this is the guard F5 asked for — during phase 2 the admitted calls are
still writing through the in-memory handles, so a guard on "Close started"
permitted exactly the window it was documented to prevent.

`746cf4dfb` is a one-line lint fix (`Ping` → `PingContext`) on the tests
`692b28541` added, caught by the pre-push hook rather than CI. Correct.

`430c79a6e` closes the documentation half of R3-5. F4 subsequently found
four per-method godoc paragraphs (`Run`, `RunWithCredentials`, `Messages`,
`Session`) still promising that "an admitted Run always finishes against a
fully live App", which is false on the forced path; `066dd6ad7` fixes those
too. Both dispositions are cited from the fh report and spot-checked
against the diffs.

## 2. Multi-tenant credentials (`97ffd22ff`, `4f88867a5`, `752a811d3`)

`97ffd22ff` is the largest single feature commit in the range and the one
whose commit message makes the strongest claims. Two of those claims are
about pre-existing races it fixed rather than about the feature, and both
check out on re-derivation:

- **Shared tool mutation.** `runTurn` used to call
  `agentTools[len-1].SetProviderOptions(...)` on an element of `a.tools`.
  `csync.Slice` protects the slice header, not the pointed-to tool objects,
  and the slice is name-sorted so two concurrent turns pick the *same* last
  element. The replacement (`internal/agent/tool_provider_options.go`,
  `withProviderOptionsOnLast`) clones the slice and wraps only the last
  element, so nothing shared is written. This also removes the hidden
  dependency where `PrepareStep`'s `a.tools.Copy()` inherited the marker
  only because the shared object had been mutated.
- **`errgroup` reuse.** `coordinator.readyWg` was an `errgroup.Group`
  registered from `NewCoordinator` *and* from every `UpdateModels`, while
  run entry points sat in `Wait`. That violates both documented errgroup
  constraints and is a genuine `sync.WaitGroup` "Add concurrently with
  Wait" hazard, not just a `-race` artifact. `readyGate` (counter +
  `sync.Cond`) has the right semantics, tolerates re-entrant registration
  (`buildAgent`'s tool task registers two more tasks on the same gate),
  and releases the pending slot in a `defer` so a panicking task cannot
  wedge a concurrent `Wait`.

One design property worth recording: `readyGate.Wait` keeps the *first*
error forever, matching the errgroup it replaced. Every run entry point
(`Run`, `RunWithCredentials`, the interrupt paths) returns that error, so a
single sub-agent build failure bricks the coordinator for the process
lifetime. That is pre-existing behaviour faithfully preserved, and the fh
report already noted it; I mention it because `9b3841e0c` later adds new
`readyWg.Wait()` trigger sites inside `pinCallTools`, widening the set of
ways to arm it.

`4f88867a5` makes `CredentialSet.Validate` fail closed (known roles, known
provider types, mandatory smart role) and adds the explicit
`AllowConfiguredRoleFallback` opt-in. This is a real closure of R1-5: the
`Role("smrat")` typo that used to silently route a tenant's whole prompt
through the operator's provider now fails validation. See C1-10 for a
cosmetic error-text mismatch in the same function.

`752a811d3` closes F1 (`RunWithCredentials` never got a pinned toolset
after R3-1, so `DisableSubAgents`/`ModelRole` were silently ignored on the
SDK's flagship path). Ten lines in `resolveCredentialsModels`; correct.

## 3. Library mode (`c0d542986`, `9e45f1824`, `ae277c26b`, `887359735`)

`9e45f1824` tolerates only `ENOENT` on `os.Stat(workDir)` and `MkdirAll`s;
any other stat error and a non-directory path still fail. Correct and
minimal.

`c0d542986` builds a zero-disk-I/O `ConfigStore` from `LibraryConfig` and,
for an empty `WorkingDir`, an in-memory SQLite database behind a
keeper+main handle pair. The keeper pattern is right (a named shared-cache
memory database dies when the last connection closes) and `closeConns` is
ordered main-first/keeper-last. Two observations:

- The fixed DSN (`file:rush_sdk_memory?mode=memory&cache=shared`) made
  every ephemeral client in a process share one database and re-run
  migrations onto it — R1-2, closed by `4f88867a5`'s `newMemDSN()` (UUID
  per client). Verified present at the tip (`sdk/library_mode.go:471`).
- The commit uses the package-global goose API (`goose.SetDialect` +
  `goose.UpContext`) while `internal/db` reaches the same globals through
  its own `initGoose()` `sync.Once`. See C1-12; closed at the tip, which
  calls `db.Migrate(ctx, main)` instead (`sdk/library_mode.go:559`).

`ae277c26b` adds `Messages`/`Session` as thin pass-throughs. At
introduction they had no closed-client guard; `4f88867a5` adds
`ErrClientClosed` to both. Neither has a tenant-ownership check — that is
the documented, accepted R1-6 position (authorization is the host's job),
not an oversight.

## 4. Origin tagging (`8f2f71043`, `9b9ec295a`)

The migration (`ALTER TABLE ... ADD COLUMN origin TEXT DEFAULT '' NOT
NULL` on both tables) is valid for SQLite because the default is a
constant; the `Down` uses `DROP COLUMN`, which needs SQLite ≥ 3.35 — fine
for the modernc driver in use.

Column-order wiring was checked by hand: `CreateSession`'s column list
appends `origin` after the literal `yolo_enabled` value, and the Go arg
list appends `arg.Origin` after `arg.FastModelID`, so the binding lines up.

The Go-side threading is coherent and I could not find a hole:
`RunRequest.Origin` → `agent.WithCallOrigin(ctx)` in `ExecuteRun`
(`internal/app/app_run.go:582`) → read by `resolveSession`
(`:261`, only on the two branches that actually *create* a session) →
`buildCall`/`runInternal` stamp `SessionAgentCall.Origin`
(`internal/agent/coordinator_run.go:136`, `:291`) → `createUserMessage`
persists it. `InjectMessage` goes through `buildCall`, so the web
inject path's `WithCallOrigin` is not dead code (I checked:
`coordinator_interrupt.go:786` → `agent_control.go:90`). The durable
queue mirrors it (`session.SessionAgentCallData.Origin`), and
`ForkSessionTx` copies both session and per-message origins.

The `OriginCreator` consuming-interface seam (type-assert, fall back to
the legacy `Create`) is the same pattern used for `credentialRunner`;
it keeps `session.Service`'s many test fakes compiling. Its cost is that a
fake that does *not* implement it silently produces `OriginUnspecified`
rows, which is the safe direction.

`9b9ec295a` is a comment reword to dodge a gofumpt rewrite of the literal
two-quote sequence. Cosmetic and correctly explained.

## 5. Per-call execution context and admission (`e900aea0a`, `c335537f2`, `cf0dfc64f`, `8474beb91`, `8341077dc`, `4f15041d3`, `f24f17463`, `ddaccc1a6`, `271550b1f`)

This is the R1-1/R1-4 → R2-1/R2-3 → R3-3/R3-4/R3-6 → F2/F3/F6 → R4-1/2/3
chain. Rounds 3, 4, 5 and the fh re-read cover it in depth and I do not
re-derive it wholesale. Points I did verify independently:

- **`mailbox.submit`'s fail-fast branch** (`internal/agent/mailbox_ownership.go:67-69`)
  sits inside the same `mb.mu` hold as the `mbIdle` check, so two
  simultaneous starters cannot both observe idle. It is placed *before*
  the `FromDurableQueue` guard, so a hypothetical fail-fast durable call
  would also not be enqueued — which happens to be the same outcome the
  durable guard wants, so the ordering is harmless.
- **`f24f17463`'s F6 fix** adds a single `admissionAborted` bool set inside
  `checkHoldCanceled` and checked by both deferred cleanup closures. Both
  the setter and the readers run on `ExecuteRun`'s own goroutine (deferred
  closures always do), so the plain bool is correct — no atomic needed, as
  the comment claims.
- **`f24f17463`'s F3 fix** ORs a new `RunOverrides.TimeoutOptionsSet` into
  the derived predicate. `sdk.RunOverrides` is an alias of
  `app.RunOverrides`, so the field really is reachable from an external
  embedder; F3 is genuinely closed as an API matter. It remains
  behaviourally unobservable today because `SetAgentTimeoutOptions` has no
  production caller and `buildAgent` never sets the shared fields — the fh
  report says so, and the commit message does not overstate it.
- **`ddaccc1a6` is superseded, not merely improved.** Round 4 rates its
  session-level "durable-restart baseline" as two P0 same-process
  permission bypasses (R4-1: last-queued policy wins for every durable row;
  R4-2: a baseline-governed parent hands its sub-agent auto-approval with
  no restriction, because `InheritSessionRunAllowlist` reads only
  `runAllowlistBySession`). `271550b1f` replaces the baseline with a
  serialized `RunAllowlistSpec` bound by `LogicalCallID`, which round 5
  confirms closes R4-1…R4-3 for newly written rows. Anyone reading the
  history should treat `ddaccc1a6` as a step that must not be reverted to.

`c335537f2` deserves one note of its own: `sdk.Client.Run` and
`RunWithCredentials` set `req.FailIfSessionBusy = true` **unconditionally**,
overwriting whatever the caller put there. There is no SDK path to the
queueing behaviour. That is documented in the godoc and is a defensible
product decision, but it means the `RunRequest` field is not a knob for SDK
users, only for in-repo `app` callers.

`6a1c5fbd5` — the follow-up that made the legacy queueing path stop
reporting `ErrSessionBusy` — is where I disagree with the fix; see C1-1.

## 6. Scoped `fs_*` tools, T1–T6 (`232e62c42`, `4fc762ccf`, `887fc9f63`, `50e6dd687`, `3f007e6b3`, `7b928bb4b`)

About 5,000 added lines, all new files plus a small refactor of
`grep.go`/`rg.go`. Round 5 covers this group from the SDK-contract angle
(R5-1, R5-2, R5-5); this section is the code-level read.

**What is solid.** The matcher (`internal/permission/folderscope.go`) is a
genuinely careful piece of work. The zero value denies everything; any
malformed entry fails the *whole* compilation rather than being dropped,
with an explicit and correct justification (a dropped deny carve-out would
widen access, a dropped grant would narrow it, and the compiler cannot tell
which the host meant). Longest-dir-first ordering is sound because for
nested entries the deeper directory is always the longer string, and
non-nested entries can never both contain the same path. Containment uses
`filepath.Rel` + `!strings.HasPrefix(rel, "..")`, which on Windows is
case-insensitive (Go's `Rel` compares segments with `sameWord`, i.e.
`strings.EqualFold`, on that platform) — so a drive-letter/case variation
matches, and an 8.3 short-name spelling fails closed. Both are the safe
directions.

The batch runner's three-phase policy (shape → pure preflight → best-effort
grouped execution) is consistent across all eight tools, groups by resolved
path so N edits to one file produce one atomic write and one history
version, and never sets `StopTurn` for a per-item denial. `fs_write_lines`'
bottom-up application with all-members-of-an-overlapping-pair rejection is
correct and the reasoning in the comment (determinism over lossiness)
holds.

**Where the design has holes** — three of them, all named by later reviews,
all originating in this range:

- The scope's entry directories were compiled **lexically**
  (`filepath.Join` + `filepath.Clean`) while every requested item path went
  through `resolveScopedPath`'s symlink resolution. Two namespaces on the
  two sides of the same `Check`. That is R5-2 (P0), closed later by
  `e567dd48a`'s `CanonicalizeFolderScopeSpec`. What is new here: T2's own
  test already encodes the mismatch — see C1-6.
- `fs_list`/`fs_find` scope-checked their *result* entries with
  `filepath.Clean(native)`, again lexically (R6-2, closed by `87e9c72cb`).
- `fs_write` treated a failed old-content read after a successful `Stat` as
  an empty baseline (R5-5, closed by `83ca7e831`; the tip now returns
  before `WriteFile`, `internal/agent/tools/fs_write.go:218-235`).

**Checked and found clean.** I specifically looked for a
dangling-symlink create escape: `resolveScopedPath` reports a dangling
final component as non-existent, so the scope check judges
`<resolvedParent>/link`, which is inside the grant, while a naive
`os.WriteFile` would follow the link outside it. It is not exploitable,
because the write path is `commitFileChange` → `fsext.AtomicWriteFile`,
whose `os.Rename` replaces the symlink itself rather than its target
(`internal/fsext/atomic.go:51`). `fs_delete`/`fs_replace`/`fs_write_lines`
all operate on the already-resolved path, so an existing symlink pointing
outside the scope is denied before any I/O. Worth recording because the
safety here comes from `AtomicWriteFile`'s implementation, not from any
explicit `O_NOFOLLOW`/`Lstat` — there is none anywhere in the family, so
swapping the write primitive would reopen it.

The `rg.go` extraction in T5 (`appendRgIgnoreFiles`, `buildRgSearchCmd`)
is byte-equivalent for the legacy caller: `getRgSearchCmd(..., 0)` produces
the same argv, and the ignore-file loop was moved verbatim. The new
`-C N` flag is appended after the positional pattern, which ripgrep accepts
(interspersed args), so ordering is fine.

## Memory, goroutines and resources

Nothing in this range leaks in a way I could substantiate. Specifically:

- `ShutdownWithResult`'s cleanup wait spawns one goroutine to `wg.Wait()`
  and closes `waitDone`. On the 10s outer-timeout path that goroutine is
  abandoned but is guaranteed to finish once the cleanups return, and it
  writes to nothing the caller reads (the error slice is mutex-guarded and
  snapshotted). No leak.
- `readyGate.Go` always decrements `pending` in a `defer`, including on a
  panicking task, so a concurrent `Wait` cannot be wedged. `Broadcast` only
  on the `pending == 0` transition is correct because every waiter's
  predicate is `pending > 0`.
- `openMemoryDB` closes both handles on every error path after they are
  opened (pragma failure, migration failure). `openLibrary` closes
  `closeConns` and releases the pooled reference on `app.New` failure. The
  ephemeral client's `dataDir` is `""`, so `ShutdownWithResult` skips
  `db.Release` and `Client.Close` owns the handles — the ownership split is
  explicit and consistent.
- `runRipgrepContextSearch` drains stdout before `Wait` on every early-stop
  path (budget spent or parse error), which is the `os/exec` contract that
  prevents the child blocking on a full pipe.
- `fsGrepRunItem` defers its per-item `cancel()`. `resolveScopedPath`'s
  walk-up loop terminates at the volume root (`filepath.Dir(prefix) ==
  prefix`).
- One bounded-growth nit, not a leak: `scanFileWithContext` inserts an
  empty collector into `files` for **every** file it opens, before knowing
  whether it matches (`internal/agent/tools/fs_grep.go:421`). On a large
  tree that is one map entry per scanned file for the duration of the item.
  Rendering skips them, so it is memory only. Folded into C1-8.

## Findings

### C1-1 — P1: a legacy queued `ExecuteRun` now reports success with an empty envelope (`6a1c5fbd5`)

`runAgentTurnRecovered`'s `result == nil, err == nil` branch used to return
`fmt.Errorf("failed to start agent processing stream: %w", agent.ErrSessionBusy)`.
`6a1c5fbd5` replaces it with `done <- agentTurnResponse{}`
(`internal/app/app_run.go:432`). The diagnosis is right — that mapping was
wrong, because `sessionAgent.Run`'s only `(nil, nil)` return is the legacy
queueing branch of `mailbox.submit`, and every genuine fail-fast rejection
already carries a non-nil `ErrSessionBusy`. The *replacement* is the
problem.

The consumer is `case result := <-done:` at
`internal/app/app_run.go:1476`, which calls `finish(nil)`. With
`runErr == nil` and `finalReason == ""` (no assistant message ever
arrived — the turn has not started), `runFailed` is false, so `finish`
takes `hookExitReason = "stop"; return nil, nil`
(`internal/app/app_run.go:1408-1409`). In `RunModeJSON` the same state
produces a `RunResult` with `final_text: ""` and a success exit reason.

Concrete failure: `rush run --session S "<prompt>"` in a process whose
`RunQueuePump` already owns `S` (a durable row left by a previous
`sessions inject --interrupt`, or an earlier orphaned call) submits into a
busy mailbox, is queued, and returns immediately. The CLI exits 0, prints
nothing, and the JSON envelope says the run stopped normally. The prompt
*will* run later under the owner's dispatcher loop, but the caller — for
this fork, typically an orchestrating agent — has already been told the
work is done. Before this commit the same situation produced a loud
non-zero exit with `sessionBusyGuidance` printed to stderr.

Neither outcome is right; the missing third option is to report the queued
state. `RunResult` already has `Warnings` and an `ExitReason`, so
`finish` has somewhere to put "queued behind the current owner, not
executed in this invocation".

Test coverage does not reach it: the modified
`TestRunAgentTurnRecovered_NilResultNoError` pins only
`require.NoError(resp.err)` + `assert.Nil(resp.result)` on the helper. No
test asserts what `ExecuteRun` then returns to its caller, so the silent
success is unpinned in either direction.

### C1-2 — P1 at the time, closed at tip: `pinCallTools` fails open to the shared toolset (`9b3841e0c`)

At `9b3841e0c`, `pinCallTools` returned a bare `nil` slice on three
distinct failures — coder agent unconfigured, `buildTools` error, ready-gate
error — and `resolvedOverrides.pin` skips `Tools` when the slice is nil, so
the call silently reverted to `a.tools`: the shared, *unfiltered* toolset
built at coordinator construction. The `slog.Warn` is the only signal.

Concrete failure at that commit: a run with `--agents single`
(`CallOptions.DisableSubAgents = true`) whose `buildTools` hits a transient
error — e.g. a failing MCP registry read inside `GetMCPTools` — gets the
`agent` and `agentic_fetch` delegation tools back in the provider request.
The operator explicitly asked for no sub-agents and gets them, with no
error and no envelope warning. The same holds for `ModelRole`-driven
orchestrator tool stripping.

Round 5 later escalated this to R5-1 (P0), because folder-scoped calls hit
the same nil path and fall back to an **unscoped** toolset. The tip has the
fix: `pinCallTools` now returns `([]fantasy.AgentTool, error)` and answers
with `ErrScopedCallToolsUnavailable` whenever the context's `CallOptions`
requires a distinct toolset (`internal/agent/coordinator_models.go:578-608`),
with the legacy `(nil, nil)` behaviour retained only for callers that asked
for nothing. Recorded here because the fail-open was introduced by a commit
in this range and its `--agents single` half was already a real bypass
before folder scopes existed.

### C1-3 — P2: the tool-timeout stop tells the orchestrator to raise a flag that did not fire (`7dd4a7d23`)

`agent_turn.go` appends `WatchdogResumeGuidance(call.SessionID, flag)` to
the watchdog finish body for two causes
(`internal/agent/agent_turn.go:1813-1819`):

```go
if cause == causeToolTimeout || cause == causeHardCap {
    flag := "--timeout"
    if cause == causeHardCap {
        flag = "--timeout-hard-cap"
    }
    body = fmt.Sprintf("%s\n\n%s", body, WatchdogResumeGuidance(call.SessionID, flag))
}
```

`causeHardCap` → `--timeout-hard-cap` is correct: that is literally the
limit the watchdog compares against (`a.timeoutHardCap`, set from
`RunOverrides.TimeoutHardCap` / the `--timeout-hard-cap` flag).

`causeToolTimeout` → `--timeout` is not. That cause fires on
`effectiveToolMaxDuration()`, which is `toolExecutionMaxDefault` (45m)
overridden only by `a.toolMaxDuration`, which comes from
`SessionAgentOptions.ToolMaxDuration` ← `Options.StreamToolTimeoutSeconds`
(`internal/agent/agent_timeouts.go:17-34`,
`internal/agent/coordinator_tools.go:56-57`,
`internal/config/config.go:469`). That is a `rush.json` setting
(`options.stream_tool_timeout_seconds`), not a CLI flag, and `--timeout`
is the run's wall-clock deadline, an entirely different limit.

Concrete failure: a single tool (typically an `agent` delegation) runs past
45m. The turn stops with `title="Tool timeout"`, and the appended guidance
says `rush run --session <id> --timeout <larger-value> "continue"`. An
orchestrator that follows it re-runs with a bigger wall-clock budget, hits
the same unchanged 45m tool cap, and stops again identically. The message
promises a resume that cannot work.

It also contradicts the body it is appended to:
`watchdogFinishMessage`'s `causeToolTimeout` branch
(`internal/agent/stream_watchdog.go:499-502`) advises "Re-run the step; if
it's a long job, poll its status instead of blocking" — deliberately *not*
offering a timeout knob.

Existing tests do not catch it: `internal/agent/agent_timeouts_test.go`
exercises `watchdogFinishMessage` directly, before the concatenation, and
`timeout_stop_test.go` only checks that the flag string it was handed is
substituted into the command.

Minimal fix: either drop the appended guidance for `causeToolTimeout`
(leaving the existing, accurate body), or give `WatchdogResumeGuidance` a
config-knob variant that names `options.stream_tool_timeout_seconds`.

### C1-4 — P2: `fs_read`'s `lines=` range is one too high for newline-terminated files (`50e6dd687`)

```go
lastLine := win.firstLine - 1
if content != "" {
    lastLine = win.firstLine + strings.Count(content, "\n")
}
```
(`internal/agent/tools/fs_read.go:189-192`)

`readTextFileFrom` returns `strings.Join(lines, "\n")`, so
`Count(content, "\n") == len(lines)-1` and the arithmetic would be right —
except that `readTextFileFrom`'s read loop appends the empty segment after a
file's final newline as a line: for `"alpha\nbeta\n"` the loop reads
`"alpha\n"`, `"beta\n"`, then `("", io.EOF)` and appends `""` before
breaking (`internal/agent/tools/view.go:377-399`). `lines` is therefore
`["alpha","beta",""]`, `content` is `"alpha\nbeta\n"`, `Count` is 2, and
`lastLine` is 3 for a two-line file.

Concrete failure: `fs_read {"items":[{"path":"a.txt"}]}` on a normal,
newline-terminated source file emits
`<file path="a.txt" lines="1-3" status="ok">` plus a phantom
`     3|` line from `addLineNumbers` (`view.go:318-341`, which splits on
`"\n"` and numbers all three segments). A model that trusts the advertised
range and follows up with `{"path":"a.txt","start_line":3,"end_line":3}`
gets an empty window.

The whole test file avoids the case by construction: every fixture writes
`strings.Join(lines, "\n")` with no trailing newline
(`internal/agent/tools/fs_read_test.go:27`, `:107`, `:167`), which is
the one spelling for which the arithmetic is correct. The phantom line is
pre-existing `view.go` behaviour, but `fs_read` is the first consumer to
publish an explicit line range derived from it.

### C1-5 — P2: the batched `-race` CI step passes green if the test-name extraction returns nothing (`198c3d52f`)

`.github/workflows/build.yml`, `test-race` job, second step:

```bash
set -uo pipefail
names=()
while IFS= read -r name; do names+=("$name"); done < <( grep -h "^func Test" internal/agent/*_test.go | sed ... | LC_ALL=C sort -u )
total=${#names[@]}
size=$(( (total + batches - 1) / batches ))
for ((i = 0; i < batches; i++)); do
  chunk=("${names[@]:start:size}")
  [ ${#chunk[@]} -eq 0 ] && continue
  ...
done
```

If the `grep` produces no names — the glob stops matching after a package
move, a rename to a different suffix, `grep`'s exit status is ignored
anyway because it feeds a process substitution — then `total` is 0, `size`
is 0, every chunk is empty, every iteration `continue`s, `status` stays 0
and the step reports success **having run zero tests**. The only
`internal/agent` `-race` coverage in CI would then be silently gone.

This is exactly the failure class `7d8571ed3` fixed 15 minutes earlier the
same day in `check_db_release_pairing.sh` ("refusing to report a false
pass"), applied to a listing that feeds a CI gate rather than a hook. A
`[ "$total" -eq 0 ] && { echo "no tests extracted"; exit 1; }` closes it.

Smaller, same step: `grep "^func Test"` also matches
`func TestMain(m *testing.M)` — `internal/agent` has one
(`internal/agent/agent_test.go`, per `agent-leak-smoke.yml`'s own comment)
— so `TestMain` occupies a slot in the `-run` alternation. Harmless, but it
skews the batch sizes by one.

The pre-existing `test-agent` job uses the same extraction technique and
has the same fail-open shape; this commit propagates it rather than
introducing it.

### C1-6 — P2 at the time, closed at tip: T2's own test encodes the R5-2 namespace mismatch (`4fc762ccf`, with `232e62c42`)

`TestResolveScopedPath_SymlinkEscapeResolvesOutside` (added by
`4fc762ccf`, `internal/agent/tools/fs_scope_test.go`) builds a scope from
an **unresolved** directory:

```go
scope, _ := permission.BuildFolderScope(permission.FolderScopeSpec{
    WorkingDir: tmp,
    Entries: []permission.FolderScopeEntry{{Dir: filepath.Join(tmp, "root"), ...}},
})
wantInside, _ := filepath.EvalSymlinks(real)
require.NoError(t, scope.Check(wantInside, permission.FileOpRead))
```

and then queries it with a **symlink-resolved** path. That is precisely the
two-namespace mismatch round 5 named R5-2 (P0): production compiled entry
dirs with `filepath.Join`+`Clean` only
(`internal/permission/folderscope.go:196-204`) while every requested item
went through `resolveScopedPath`'s `EvalSymlinks`.

The assertion passes on Linux and Windows because `t.TempDir()` is not a
symlink there. On macOS, where `t.TempDir()` lives under `/var/folders/...`
and `/var` is a symlink to `/private/var`, `wantInside` is
`/private/var/...` while `entry.dir` is `/var/...`, `filepath.Rel` yields a
`..`-prefixed result, and the `require.NoError` fails. The test that would
have surfaced R5-2 before it shipped was written, ran, and passed for a
platform-specific reason.

The tip closes the production bug via
`tools.CanonicalizeFolderScopeSpec` (`e567dd48a`,
`internal/agent/tools/fs_scope.go:161-201`), which resolves `WorkingDir`
and every entry through the *same* resolver and provider as item paths.
Recorded because the two commits that created the gap are in this range and
because the fixture shape is worth not repeating.

### C1-7 — P2: `fs_grep` ignores the configured `tools.grep.timeout` (`3f007e6b3`)

```go
searchCtx, cancel := context.WithTimeout(ctx, config.ToolGrep{}.GetTimeout())
```
(`internal/agent/tools/fs_grep.go:129`)

`config.ToolGrep{}` is a zero value, so `GetTimeout()` always returns the
hardcoded 5s default (`internal/config/config.go:690-692`) and
`options`/`tools.grep.timeout` from `rush.json` is never consulted. The
legacy tool does it properly — `NewGrepTool(workingDir, config
config.ToolGrep)` and `config.GetTimeout()` at
`internal/agent/tools/grep.go:169`, `:185` — and the constructor site has
the value in hand: `coordinator_tools.go:658` passes `cfg.Tools.Grep` to
`NewGrepTool` twenty lines above `NewFSGrepTool(c.cfg.WorkingDir(), scope,
disk)` at `:678`, which takes no config at all.

Concrete failure: an operator with a large repository sets
`{"tools":{"grep":{"timeout":"60s"}}}` because 5s was not enough. `grep`
honours it; `fs_grep` does not. In a folder-scoped SDK call the legacy
`grep` is stripped from the toolset, so `fs_grep` is the *only* content
search available and it silently caps at 5s — the search returns
`error searching files: context deadline exceeded` on exactly the
configuration the operator changed to prevent that.

Not reported by any existing review document (checked: no `docs/reviews/*`
file mentions `ToolGrep`).

Fix: give `NewFSGrepTool` a `config.ToolGrep` parameter like its sibling.

### C1-8 — P3: `FSBatchMaxReadOutput` is weaker than its documentation (`887fc9f63`)

`FSBatchMaxReadOutput` is documented as "the total read output one call may
emit across all items" (`internal/agent/tools/fs_batch.go:81-83`). It is
enforced only *between* execution groups:

- the check is `if emitted >= FSBatchMaxReadOutput` at the top of the group
  loop (`:335`), so the group that crosses the threshold still emits in
  full — the real bound is `budget + one group's output`;
- `emitted` only accumulates for `FSStatusOK` outcomes (`:379-384`), so an
  `Execute` that returns a `Block` alongside a non-OK status contributes
  rendered bytes that are never counted. No current tool does this, but
  nothing in the `FSItemOutcome` contract forbids it.

Same commit, folded in: `scanFileWithContext` (T5,
`internal/agent/tools/fs_grep.go:420-421`) inserts an empty collector into
the shared `files` map for every file it opens, before knowing whether the
file matches. On a large tree that is one map entry per scanned file for the
duration of the item; `fsGrepRender` skips zero-hit collectors, so it is
memory pressure only, not an output bug.

Also in the same family: `fs_grep`'s timeout is per **item**
(`fsGrepRunItem` creates it), and `RunFSBatch` has no batch-level clock.
With `FSBatchMaxItems = 50` distinct roots, one `fs_grep` call can occupy
50 × 5s ≈ 250s. That is well inside the 45m tool watchdog, so it is a cost
note rather than a hang, but the batch caps section does not mention time.

### C1-9 — P3: `df6b2370c`'s "pure regrouping" is not quite pure

The commit message says "Pure regrouping: `-short -failfast -p 2`
unchanged, no coverage change." The flags are indeed unchanged and coverage
is preserved (the computed catch-all step is a good touch). But the old
single step was `go test -p 2 $(go list ./... | grep -v '/internal/agent$')`
— one invocation with `-p 2` scheduling across the *whole* package set. The
new form is nine sequential steps, each with `-p 2` inside itself, so
cross-group parallelism is gone and the job's wall-clock grows. Worth one
line in the message, since CI duration is the thing the surrounding
comments in this file spend most of their words on.

### C1-10 — P3: misleading error text on an unreachable branch (`4f88867a5`)

```go
if !smartCovered {
    return nil, fmt.Errorf("credential set does not cover the smart role (Models[RoleSmart]) and AllowConfiguredRoleFallback is false; ...")
}
```
(`internal/agent/credentials.go:343`)

The guard is unconditional, so the message asserts
`AllowConfiguredRoleFallback is false` even when the caller set it to
`true`. In practice the branch is unreachable — `Validate` already rejects
a `CredentialSet` without `RoleSmart` (`credentials.go`, the
`must define the smart role` check), and both `RunWithCredentials` and
`ExecuteRun` validate before resolving — so this is defence-in-depth with
wrong prose. A consequence worth noting: because `smartCovered` is
guaranteed true at that point, the following
`if creds.AllowConfiguredRoleFallback && (!smartCovered || !fastCovered)`
reduces to `AllowConfiguredRoleFallback && !fastCovered`; the
`!smartCovered` disjunct is dead.

### C1-11 — P3: `RunWithCredentials` takes `CredentialSet` by value but shares its maps (`97ffd22ff`)

```go
func (c *Client) RunWithCredentials(ctx context.Context, req RunRequest, creds CredentialSet) (*RunResult, error) {
    ...
    req.Credentials = &creds
```
(`sdk/sdk.go`)

The by-value parameter copies the struct header, so `Credentials []Credential`
and `Models map[Role]ModelChoice` still alias the caller's backing storage.
`CredentialSet`'s godoc says "Treat the value as immutable after
construction", but a host that reuses one `CredentialSet` across calls and
mutates `Models` between them races the coordinator's reads (the set rides
the call context into `runSubAgent`'s `callCredentialsFrom` and the 401
rebuild, both of which can run long after `RunWithCredentials` returned to
the caller's frame). The by-value signature reads like a defensive copy and
is not one. Either deep-copy on entry or document that the maps must not be
touched for the lifetime of the call.

### C1-12 — P3, closed at tip: library mode wrote goose's package globals (`c0d542986`)

`openMemoryDB` called `goose.SetDialect("sqlite3")` and
`goose.UpContext(ctx, main, "migrations")` — the package-global goose API —
with the comment "SetDialect is idempotent". It is idempotent in effect but
not race-free: `SetDialect` assigns goose's package-level store, and
`internal/db` reached the same globals from `initGoose()` under its own
`sync.Once` (`internal/db/connect.go:336-342` at that commit). An
application-mode `db.Connect` running concurrently with a library-mode
`sdk.Open` in the same process is an unsynchronised write/write on that
global.

Closed at the tip: `openMemoryDB` now calls `db.Migrate(ctx, main)`
(`sdk/library_mode.go:559`) and `internal/db` uses
`goose.NewProvider(goose.DialectSQLite3, ...)` instead of the globals. The
commit's claim that "internal/db's package init already pointed goose at
db.FS" was accurate *at that commit* (there was a `func init()` calling
`goose.SetBaseFS`); it is no longer true at the tip, which is worth knowing
if anyone reads that comment archaeologically.

### C1-13 — P3: the grep-cancellation test is still timing-derived (`591b649a6`)

The added warm-up walk removes the specific Windows-CI failure (a cold
2.1s baseline followed by a much faster warm walk that finished before
`cancel()` fired). But the test still computes its cancel delay as
`fullElapsed/4` from a measured baseline and then races a third walk
against it (`internal/agent/tools/grep_fallback_test.go`, the block around
the warm-up). Any run where the third walk is more than 4× faster than the
second reproduces the same "error expected, got nil". A deterministic
version would gate cancellation on an observed walk event rather than a
wall-clock fraction. The commit's own diagnosis is accurate and the fix is
a strict improvement; this is a note that the class of flake is reduced,
not eliminated.

### C1-14 — P3: the new pairing self-test has no caller (`7d8571ed3`)

`.githooks/check_db_release_pairing_selftest.sh` is added with a good mock
harness (a PATH-shadowing `git` that forces `grep` exit codes while
delegating everything else to the real binary) and three cases. Nothing
invokes it: `.githooks/pre-push:217` runs
`check_db_release_pairing.sh` but never the self-test, and no workflow
under `.github/` references it (grep over the whole tree finds only
self-references). It will rot silently the first time the guard's internals
change. Either wire it into pre-push (it costs three `bash` invocations) or
say in its header that it is a manual tool.

### C1-15 — P3: four cited review reports are unreachable from `main`

`e900aea0a`, `4f88867a5`, `f4e3fd86a`, `cf0dfc64f`, `9b3841e0c`,
`692b28541`, `8474beb91`, `430c79a6e`, `8341077dc` and `4f15041d3` all name
findings R1-x/R2-x/R3-x in their subject lines. The reports defining those
identifiers live only on `research/upstream-gap-analysis-20260830`
(`ad3bca8ba`, `31d8ed88f`, `937df4566`, `eb94b67ed`) and are absent from
`main`'s `docs/reviews/`. R4-4 raised the same complaint about
`2026-09-01-sdk-review-fh.md` and was closed for that one file by
`f4e30ce4f`; rounds 1–4 were not brought over. Cherry-picking those four
documentation-only commits onto `main` would make the whole arc auditable
from a clean checkout.

---

## Open findings

| ID | Sev | Commit(s) | Area | Live at tip? |
|---|---|---|---|---|
| C1-1 | P1 | `6a1c5fbd5` | queued `ExecuteRun` returns success with an empty envelope (`internal/app/app_run.go:432` → `:1476` → `:1408`) | yes |
| C1-2 | P1 | `9b3841e0c` | `pinCallTools` fail-open to the shared unfiltered toolset | no — closed by R5-1/R6-3 fixes (`coordinator_models.go:578-608`) |
| C1-3 | P2 | `7dd4a7d23` | tool-timeout stop suggests `--timeout`, not `stream_tool_timeout_seconds` (`agent_turn.go:1813-1819`) | yes |
| C1-4 | P2 | `50e6dd687` | `fs_read` `lines=` off-by-one + phantom line (`fs_read.go:189-192`) | yes |
| C1-5 | P2 | `198c3d52f` | batched `-race` step passes green on an empty test-name list (`.github/workflows/build.yml`) | yes |
| C1-6 | P2 | `4fc762ccf`, `232e62c42` | T2 test builds the scope from an unresolved dir (the R5-2 mismatch) | production bug closed by `e567dd48a`; fixture shape unchanged |
| C1-7 | P2 | `3f007e6b3` | `fs_grep` ignores `tools.grep.timeout` (`fs_grep.go:129`) | yes |
| C1-8 | P3 | `887fc9f63`, `3f007e6b3` | read-output budget weaker than documented; per-file collector for non-matching files; no batch-level time budget | yes |
| C1-9 | P3 | `df6b2370c` | "pure regrouping" omits the loss of cross-group `-p 2` | yes |
| C1-10 | P3 | `4f88867a5` | error text claims `AllowConfiguredRoleFallback is false` unconditionally (`credentials.go:343`) | yes |
| C1-11 | P3 | `97ffd22ff` | `CredentialSet` passed by value shares its maps with the caller (`sdk/sdk.go`) | yes |
| C1-12 | P3 | `c0d542986` | goose package-global writes racing `internal/db`'s `initGoose` | no — `db.Migrate` at `sdk/library_mode.go:559` |
| C1-13 | P3 | `591b649a6` | cancellation test still derives its delay from a timing ratio | yes |
| C1-14 | P3 | `7d8571ed3` | pairing self-test has no caller | yes |
| C1-15 | P3 | ten commits | R1/R2/R3 reports absent from `main` | yes |

Severity key, matching this repository's convention: **P0** exploitable /
data-losing right now; **P1** wrong result or lost policy on a reachable
path; **P2** wrong behaviour under a specific configuration or misleading
output; **P3** hygiene, docs, latent.

---

## Verdict

This is a strong 43-commit arc and it does not need to be defended
commit-by-commit: the R1 → R2 → R3 → F → R4 chain visibly converges, each
round's fix closes the previous round's named mechanism, and by the end of
the range the same-session permission race, the shared-toolset publication,
the unbounded `Close`, the check-then-act admission window and the durable
policy loss are all genuinely closed rather than papered over. The two
pre-existing races `97ffd22ff` found and fixed (shared tool mutation, and
`errgroup` reuse under concurrent `Wait`) were real production hazards, not
`-race` noise, and the replacements are correct. The `fs_*` family's
matcher and batch-runner design is careful, and its worst holes were caught
by round 5 and round 6 within days.

The one finding I would not ship past is **C1-1**: `6a1c5fbd5` correctly
identified that mapping legacy queueing to `ErrSessionBusy` was wrong, then
replaced it with an outcome that is worse for this fork's primary consumer
— an orchestrating agent now receives exit 0 and an empty `final_text` for
a prompt that was queued and never ran, with nothing in the envelope saying
so. **C1-2** was a genuine policy fail-open for `--agents single` while it
lasted and is a good argument for keeping the tip's
`ErrScopedCallToolsUnavailable` shape permanently. **C1-3**, **C1-4** and
**C1-7** are each small, local and independently fixable, but all three
send wrong information to the model or the operator, which is exactly the
failure mode an agent-tooling CLI can least afford. **C1-5** is the one CI
item worth acting on, because a green step that runs zero tests is
indistinguishable from a green step that runs 400.

Nothing in this range shows a memory or goroutine leak, and the resource
ownership splits introduced by library mode (`closeConns` vs. the pooled
`db.Release` reference) are explicit and consistent.
