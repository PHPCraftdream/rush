# Job batches in Rush: current runtime and standalone engine

Rush selects the batch-only runtime unconditionally; since the H4 cutover there
is no legacy runtime and no selector variable. Active persistence is
`.rush/rush_store_v1.db`, initialized from one embedded initial migration. A
one-time import from untouched `.rush/rush.db` occurs before destination open.

## Runtime behavior, controls, and rollout

Import durable history, settings, queues and schedules; discard old and
preparation-era execution/control state rather than resuming it. Existing
`rush_store_v1.db` always wins and is never overwritten/refreshed, even if
malformed. Import failure does not fall back to `rush.db`.

Stop old processes and automatic restarts throughout import: host enumeration is
not a registration fence. Drain/stop all preparation hosts before upgrade.
Pre-SF bare-UUID owner-stop fences have no safely inferable provenance, and
there is no hot continuity. Requeued imported work can repeat external side
effects; this is not exactly-once execution.

Rollback requires stopping new hosts, then running the previous binary against
untouched `rush.db`; that restores the old snapshot, not post-import activity.
Preserve `rush_store_v1.db` and sidecars. There is no reverse importer or
merge-back.

Current model controls are `batch_stop` and `batch_output`; stop is a durable
cancellation request, not termination confirmation. Workers may use `batch_stop`
only for batches owned by themselves or a descendant; orchestrators retain their
existing authority to stop worker-owned batches. Unknown owners and stale runner
incarnations are refused before a stop request is written. Do not describe current
ownership using `job_kill`/`job_output` or invent cross-session controls.
For migrating configurations and prompts off the removed legacy tools, and the
semantics of the three tool-name lists (`AllowedTools`,
`permissions.allowed_tools`, `options.disabled_tools`), see
[batch-runtime-migration.md](batch-runtime-migration.md).
Historical transcript display and presentation promises remain; no signoff
reductions are implied. Detailed rollout is also summarized in the
[README](../README.md#batch-runtime-rollout-and-data).

## Layout and ownership

The standalone engine, laboratory and `batchlab` remain as documented below;
this section covers only that half of the project.

| Location | Responsibility |
|---|---|
| `internal/jobbatch/` | Pure Engine, tree policies, admission, notices, Runner and public contracts. |
| `internal/jobbatch/testdata/` | Design section-0 location for deterministic core/runtime inputs; this directory is not present in the inspected worktree. |
| `internal/jobbatch/lab/` | Scenario loader/interpreter, controlled and process executors, journal/replay. Imports the engine; never a second scheduler. |
| `internal/jobbatch/lab/testdata/scenarios/` | 49 declared scenario fixtures, including invalid-loader inputs. |
| `internal/jobbatch/lab/testdata/process/helper/` | Portable process handshake helper used by tests. |
| `cmd/batchlab/` | Command parsing and in-process `Main`; `main` wires interrupt cancellation and exits with its result. |
| `internal/jobbatch/lab/acceptance_test.go` | Integration-owner cross-slice acceptance, not an A1 slice helper file. |

Contract changes belong to the integration owner. The frozen declarations are
`internal/jobbatch/{types,events,executor,runtime_types,errors}.go` and
`internal/jobbatch/lab/schema.go`. See design sections 2, 5 and 7.2 before
changing them. Slice ownership and prefixes remain Core `coreTest*`, Runtime
`runtimeTest*`, Process `processTest*`, Laboratory `labTest*`; acceptance uses
`acceptanceTest*`. Do not independently redesign the API or add Rush dependencies.

## Optional transition commit hook

`RunnerConfig.CommitHook func(context.Context, Transition) error` requires a
positive `CommitTimeout`. Nil preserves the autonomous runtime. Hooks execute
serially in transition order; Starts/timers, replies, views, feed, notices and
Done publish only after success. Cancellation, Stop and disposal containment
do not wait for the hook.

The Engine owner accepts speculative state ahead of durable publication and
never calls or waits for the hook. One serial committer receives immutable FIFO
items captured at acceptance (records, views, snapshots, summaries and reply
fences); it never reads Engine state. The owner publishes successful items in
order and dispatches their authorized Starts, while cancellation, timers,
termination reports and physical Stop slots continue independently. Noncontainment
public acceptance pauses at 1024 outstanding publication items; Stop, host
closing and executor lifetime messages remain serviceable. View/Snapshot/feed
and Wait expose only the published projection; Add/Stop/Inject/Release eligibility
uses speculative state. Recordless replies are fenced behind earlier items.
Nil hooks retain the original inline path.

Error, panic or timeout faults the Runner: `Faulted()` closes and `FaultErr()`
wraps `ErrCommitFailed`. No further Engine event or Final is accepted. Public
calls and unpublished Wait return the fault; previously published summaries
remain valid. Events drains retained records then returns the fault, with
Final zero. Close waits for containment termination/disposal, bounded by its
caller context, and always returns the fault. Unpublished Done stays open.
A timed-out hook may still execute: the owner must not reopen the writer or
retry it and must retain ownership until process exit. Production persistence
and process-exit wiring are intentionally not included here.

## Command interface

Actual syntax from `cmd/batchlab/main.go`:

```text
batchlab run scenario.json [--events events.jsonl]
batchlab replay scenario.json events.jsonl
batchlab help
```

The run parser also accepts `--events` before the scenario path. Duplicate
journal options, extra positional arguments, or an absent required argument
are invalid. Run validates the complete scenario before opening the events
file. Replay validates metadata and applies recorded events to a fresh Engine;
it constructs no executor and does not execute scenario programs.

All current execution and verification are GitHub CI only. Prior local command examples have been removed; do not run verification locally. Current
acceptance evidence required for rollout is listed in
[jobbatch-acceptance.md](jobbatch-acceptance.md).

### Exit precedence

First matching condition wins; numeric ordering is not precedence:

| Code | Meaning |
|---|---|
| 2 | Usage, unreadable input, invalid scenario or structurally invalid journal. |
| 3 | Invocation cancellation, overall/await expiry, unfinished execution, Close timeout or cleanup failure/unconfirmed disposal. |
| 5 | Journal capture incomplete because of a gap or write error. |
| 4 | Declared action expectation or scenario oracle mismatch; undeclared action error. |
| 1 | Finished with matched expectations but at least one root not Completed. |
| 0 | Finished with matched expectations and every root Completed. |

An expected failed aggregate still exits 1. A matched Close timeout/dispose
expectation still exits 3. Exit 3 can coexist with a complete closed journal;
replay cannot recover invocation cancellation or physical cleanup failures.
For example F-e's fixture eventually closes after its earlier Close timeout;
the shortened F-e harness tests an actual unfinished capture separately.

## Scenario vocabulary and fences

Scenario authors should read `internal/jobbatch/lab/schema.go` and copy the
fenced examples `internal/jobbatch/lab/testdata/scenarios/r3-one.json` (single
item) or `internal/jobbatch/lab/testdata/scenarios/v-b.json` (Add and outcomes),
retaining their acceptance awaits and final Close before adapting the script.

The version-1 schema rejects unknown/irrelevant fields and explicit nulls where
not permitted. Required settings include limits, positive record retention and
control timeout, and an overall `wait_bound_ms`. Named payloads are `controlled`
or `process`; process payloads specify direct `program`/`args`, optional `cwd`
and an output bound, never shell text. No variable expansion occurs.

Every Submit/Add has a nonempty file-unique `ref` (at most 128 bytes of
`[A-Za-z0-9._:-]`). Ordinary Runner refs remain optional/nonunique. A raw root
task is normalized through `jobbatch.OneItem` into an ordinary parallel/all
batch; nested and Add leaves are not wrapped. Explicit batches retain policy.
There is no raw-task notification policy knob.

Actions include Submit/Add/Stop/Inject/CapacityAvailable/Release/Close,
controlled start decisions and completion, output emission/inspection,
Stop/Dispose/read gates, state/record awaits, and async close/output with join.
The loader requires the script to end in nonasync Close or the join of async
Close. Every async action must be joined. `expect_error` names typed errors,
not raw text. `expect` can pin roots/counts, node outcomes and counters,
selected notices, accepted-overlap maximum, partial-order labels and exit code.

Physical `ReleaseStart` return and report callback return are not loop
acceptance. Await Running/Started before dependent completion, inspection or
control; await terminal/Settled before dependent post-completion actions.
Observe actual held-call entry/return before pin assertions or retries.
Low-retention cases must not use trimmed feed history as a fence. Tests use
channels/barriers or virtual time rather than sleeps.

## Public API boundaries

| API | Boundary |
|---|---|
| `NewEngine`, `Engine.Apply` | Pure ordered transitions; no executor, clock or goroutine. Invalid/stale events have no effects. Direct callers transfer event ownership. |
| `Engine.View`, `Engine.Snapshot` | Current public metadata; not historical storage. |
| `OneItem` | Shared pure root-ingress helper returning a parallel/all one-task batch. |
| `NewRunner`, `Submit`, `Add` | One serialized owner of a forest/shared budgets. Runner rejects raw task roots. Add validates atomically and returns direct child IDs. |
| `Stop`, `CapacityAvailable` | Return at request acceptance, not executor termination. Repeated terminal Stop is unrecorded before Closed. |
| `View`, `Snapshot` | Serialized live reads; frozen final reads after Closed. Snapshot discovers explicit parent/ordinal ownership. |
| `Events` | Nonconsuming cursor feed. Explicit ErrGap with recovery cursor; frozen Head=Final after Closed; ErrClosed is EOF beyond Final. |
| `Output`, `Inject` | Optional leaf capabilities; physical calls pinned through actual return. Not-started, unsupported, terminal, released and closed are distinct errors. |
| `Batch.ID`, `Done`, `Wait` | Cached aggregate; Failed/Cancelled Wait outcomes have nil wait error. Cancelling Wait does not Stop work. |
| `Release` | Terminal roots only; gives up retained handles without waiting for physical pins/disposal. Keeps root tombstone and cached Wait. |
| `Close` | Latches HostClosing once, drains confirmed termination, then waits for logical disposal results within caller bound. Never synthesizes termination. |

Starting, Running and Cancelling leaves hold admission units. Stop acknowledgement,
control failure/timeout, handle pins and disposal do not release them. A report
confirms execution termination. First accepted cancellation cause wins; batch
reducers do not inherit child causes. Each live batch has a live direct child;
no terminal batch has a live descendant. Empty batches settle immediately.

Root completion selects exactly one notice. Descendants follow NotifyEach/
NotifyAll suppression, with sole-child elision per level; Wait is not another
notification. The public feed contains already-selected notices, not a request
for consumers to reapply policy.

## Rush detached terminal output cache

The Rush integration captures terminal output before a leaf reports completion and before its handle can be disposed. This cache is App-scoped and stores only copied bytes plus task/root, cursor/truncation, and finish metadata; it does not persist output or retain execution handles. Generic leaves cache the leaf `ToolResponse.Content`; shell/argv leaves cache the process merged byte stream; agent leaves cache the terminal `jobbatch.Result` summary, which is the same bytes the live `ReadOutput` returns at completion.

Each entry expires at `finished_at + 10m` (the exact boundary is expired). It retains the newest 1 MiB of task output. `oldest_cursor` is the absolute byte offset of that suffix and `next_cursor` is the absolute end offset, so callers continue by passing the returned cursor. Pages are capped at 16 KiB; `truncated` reports that earlier bytes are unavailable. The App cache is capped at 32 MiB and evicts the entry with the oldest finish time under pressure. A 1,024-entry metadata bound also limits empty outputs. Expiry is lazy on cache access; there is no expiry goroutine. Cursor semantics are per family: generic, agent and shell clamp over-end cursors to the retained end, while argv restarts from offset 0 when a cursor exceeds MaxInt64 or the retained end.

`batch_output` first uses live Runner output and falls back only when the Runner reports `ErrReleased`; successful live output is never replaced by cache data. It returns `output_expired` when no cached entry remains and `output_truncated` when the requested data starts before the retained suffix. Capture is best-effort, so pressure eviction or process exit can end availability before ten minutes. `internal/jobbatch` remains independent of this Rush-owned policy.

## Journal and replay limits

JSONL is a header, public records, and footer. The header hashes the decoded
scenario re-encoded with `encoding/json`, not the original file bytes. Submit/
Add record refs bind unique scenario actions. Records omit payloads, argv,
injected messages, output, raw errors, Result Summary and Details.

The footer separates capture (`complete`, `gap`, `write_error`) from outcome
(`closed`, `unfinished`). `final_cursor` and `final_sequence` are producer
boundaries; `last_cursor` is writer progress. Each root has its own
`settled_sequence`, which can precede the final sequence because of stateless
tails. One final `closed` record follows Final's event/commands/notices and is
last. Done publication follows transition-record publication.

Replay checks framing/digest/cursors, diagnostic inputs and request counts,
rebuilds accepted events, and compares commands/notices/Final and root
states/counts/settled sequences. It never fills missing history from snapshots.
An announced gap/write error is not a complete replay. An intact unfinished
prefix exits 3. A complete closed capture from an exit-3 invocation replays its
recorded outcome normally. Physical Stop/Dispose counters, cleanup results,
held-call return labels and output expectations are not recoverable from feed
metadata. An explicit exit-code oracle can still yield a replay mismatch.

## Historical verification limits

The execution observations recorded in the following section are historical,
not runs performed for the current H4 documentation update. They do not certify
the current integrated runtime or any platform beyond the evidence explicitly
recorded. Current gates are CI-only; see the acceptance ledger.

Observed native execution is Windows. Process tests exercise real isolated
process trees, but standalone external CLI run/replay, Unix runtime behavior,
and Rush integration are not verified by these runs. Cross-vet was not rerun
and no auditable prior command/result is attached here; it is unobserved, not a
substitute for Unix behavior. Scoped race/lint results do not certify the whole
repository. See the acceptance ledger for exact command provenance, partial
matrix rows, five restored production mutants and capture-gap exclusions.
