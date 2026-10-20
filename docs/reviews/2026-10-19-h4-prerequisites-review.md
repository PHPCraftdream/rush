## Executive summary

**Recommendation: block H4 on production recovery wiring and timeout-intent parity—not on all thirteen legacy tests. Two premises in the consultation are contradicted by the current code.**

- **Item 1:** implement recovery wiring, **P1**, **W1 → W2**; preserve recovery’s existing **wake=0** policy rather than invent autonomous recovery turns.
- **2a — `wake_only`:** implement parity, **P1**, **W3 → W2**; it is an explicit tool-schema and design contract.
- **2b — supervision debt:** **no production fix: the alleged omission is already implemented**; add a focused regression oracle, **W5**. No demonstrated P1.
- **2c — timeout presentation:** implement accurate duration/error classification, **P2, defer production changes under the Review Stop Rule**, **W4 backlog**. Do not call this a demonstrated success/failure inversion.
- **2d — silence escalation:** accept removal with legacy code, **P3**, **W5**; a new cross-process activity subsystem is not an H4 prerequisite.
- **2d — repeated question text in supervision:** accept removal of the duplicate summary text, **P3**, **W5**; the durable question notice already contains the actual question.
- **2d — idle child’s own-jobs line:** accept removal of that presentation detail, **P3**, **W5**; preserve actual child-scope liveness.
- **2e — fabricated debt from unavailable facts:** deliberate conservative fallback, but inconsistent error reporting; **defer P2**, **W5 backlog**, not a cutover P1.

**Evidence classification:** “OBSERVED” below means source or test text opened in this checkout. Failure scenarios and proposed mechanisms are **INFERRED**, not locally reproduced. No compilation, tests, database inspection, or browser execution was performed.

## Item 1 — Production dead-host recovery

### 1a. Inventory and decisions

**OBSERVED:** the broad claim “`BatchRecoverDeadHost` has no production caller” is false. `internal/session/batch_control.go:62–65` already contains:

```go
case HostStatusDead:
    _, err = s.BatchRecoverDeadHost(ctx, t.Host, probe)
    return empty, errors.Join(ErrBatchControlStale, err)
```

The real gap is **automatic and scope-triggered production recovery**, not complete absence of recovery. The “inert” comments in `batch_recovery.go:23` and `app_batch_doc.go:63–74` are stale evidence: the latter also claims installation is absent, contradicted by `app_batch_install.go:42–144`.

| Legacy location opened | Current mechanism | H4 decision / batch equivalent |
|---|---|---|
| `internal/session/async_job_store.go:265–284`, `ensureHost` | First successful registration calls `SweepDeadHosts` outside registration locks. | **Implement equivalent at App composition, W2**. Do **not** put job recovery into retained `HostStore.EnsureHost`. |
| `internal/app/app_run_async.go:311`; `internal/agent/coordinator_reaction_source.go:215–235` | Invocation-start maintenance; ON branch returns immediately at `:216–220`. | **Implement**, `BatchSweepDeadHosts` plus batch retention through a shared maintenance service. |
| `internal/agent/coordinator_recheck.go:93–118,161–180` | Existing 60-second pass invokes `RunMaintenanceSweep`. | **Implement batch branch**, reuse this scheduling surface where present; avoid duplicate App/coordinator sweep loops. |
| `internal/agent/agent_turn.go:365–384` | Turn preamble recovers before notice pull, but only OFF at `:373–374`. | **Implement batch owner-scope trigger**, before pull, bounded and conditional. |
| `internal/agent/coordinator_reaction_source.go:313–339`, `runningWorkOpen` | ON reads `LiveScopeForOwners` without recovery; OFF recovers first. | **Implement batch scope recovery**, then fresh liveness read. |
| `internal/agent/cli_active_work.go:24–49` | ON reads batch liveness; OFF calls owner recovery. | **Implement equivalent shared scope trigger**, not another independent algorithm. |
| `internal/session/async_job_store.go:307–337`, `Claim` | Recover conflicting delegation’s dead host, retry once. | **Preserve dead-conflict resolution, not legacy Claim machinery.** Recover outside Runner commit/Submit; retry the read-only attach decision once. |
| `internal/session/async_job_bgshell.go:50–72`, `ClaimShell` | Resolves a foreign shell-ID collision through recovery and one retry. | **Accept removal of this exact mechanism.** Batch execution uses root/node incarnation identities, not the legacy shell-counter key. Recovery of orphaned batch shell work remains mandatory through sweeps. |
| `internal/cmd/sessions_reset.go:175–197` | Legacy history reset recovers before activity refusal. | **Implement equivalent in the batch reset entry**, before its refusal/transaction; do not weaken Pending/Cancelling refusal. |
| `internal/cmd/sessions_jobs.go:109–139` | ON renders batch facts; OFF enumerates history. No direct recovery call here. | **Keep observation read-only**; startup maintenance may precede it, but do not add recovery to rendering. |
| `internal/cmd/sessions_gc.go:177–238` | Explicit age-based batch retention is already wired under ON. | **Retain existing mapping** to `BatchCountExpired`/`BatchPurgeExpired`; do not claim GC is a missing recovery caller. |
| `internal/cmd/sessions_reap.go:66–105` | Filesystem-only command, no DB; filters `session-*.lock`. | **No batch recovery twin required.** Do not turn `reap` into an execution-state writer. |
| `internal/server/handlers_livework.go:75–86` | Explicitly read-only; stale running work may remain until sweep. | **Keep read-only**; batch counterpart is `buildSessionLiveWorkFromBatch`, not recovery during WebSocket reads. |
| `internal/app/app_session_activity.go:109–137` | Batch activity derives live facts; no recovery there. | **Keep read-only**, serviced by production maintenance. |

Two additional qualifications:

1. **OBSERVED:** `HostStore` deliberately omitted the registration sweep: `internal/session/host_store.go:8–11,83–104`, “`HostStore.EnsureHost does not sweep`.” Restore the responsibility **above** this retained host-infrastructure layer.
2. **OBSERVED:** batch attach currently refuses foreign live ownership. `internal/session/batch_adapters.go:25–53` returns `ErrBatchForeignLiveHost`; `internal/agent/batch_install_adapters.go:239–246` merely forwards the probe. This is the concrete attach-side place to resolve a **confirmed-dead** conflict before a new Submit.

**Adversarial constraint:** do not blindly reproduce the old child-session uniqueness implementation. The opened batch schema has a live `(owner, tool_call_id)` root index (`20261012000001_batch_projection.sql:85–86`), not the old Claim contract. W2 must distinguish an actual conflicting durable identity from a merely similar child/session label.

### 1b. Recovery semantics and fences

The design authority is explicit at `docs/plans/2026-10-07-job-batches-design.md:2360–2369`:

> “Only an OS-lock confirmed-dead verdict permits the recovery sweep; unknown host status writes nothing.”

> “Sweep every nonterminal node … Pending included, to Interrupted with code `host_lost`, without rerunning side effects.”

> “For an announced root, create one root notice … preserve existing terminal state and already-selected delivery records. Delete unannounced roots…”

**OBSERVED implementation:**

- `internal/session/batch_recovery.go:27–35` skips empty/own hosts and requires both `HostStatusDead` **and no probe error**.
- `:59–75` starts a per-root transaction, elects its writer, then deletes an unannounced subtree.
- `:83–119` computes the next sequence, interrupts live nodes, selects a root notice, and closes controls.
- `internal/db/sql/batch_recovery.sql:20–26` writes:

```sql
state = 'interrupted', outcome_state = 'interrupted',
outcome_code = 'host_lost'
```

  while preserving latched causes and terminal rows.
- `:30–37` selects only a previously unselected announced root notice.
- `:39–46` answers pending controls `host_lost` or `stale`.
- `batch_recovery.go:124–127` publishes the result only after commit.

**Decision:** retain these semantics. A probe error—even if accompanied by a nominal Dead value—must write nothing. Never replace OS-lock authority with PID, elapsed time, or heartbeat expiry.

#### Important correction: recovery does not itself owe an autonomous wake

`internal/db/sql/batch_recovery.sql:34` writes:

```sql
delivery = 'pending', wake = 0
```

`internal/session/batch_recovery_doc.go:3` explicitly says recovery never “wakes work.” The existing recovery test asserts zero wakes and a non-waking pulled notice (`batch_recovery_test.go:87–111`). The scope-recovery test also expects an interruption with `wake=0` to close scope (`coordinator_scope_recovery_test.go:75–83`).

**Decision:** commit interruption and allow existing owners/waiters to observe it; do **not** change recovery notices to `wake=1` or force a paid Drain. If “wake their owners” means notification of an active waiter, send a **re-read hint**, not synthetic reaction debt. A new requirement to autonomously resume dormant owners after recovery needs an explicit design decision.

### 1c. Placement, bounds, and hot-path cost

**OBSERVED:** dead hosts intentionally remain live until recovery commits (`internal/session/batch_read_live.go:44–46`). The reader is explicitly read-only infrastructure (`internal/session/batch_read.go:12–20`).

**Recommended mechanism — W1/W2:**

1. **One App-owned recovery/maintenance dependency**, available to CLI, server, and SDK.
2. **One bounded startup pass** after the writer/data directory and host identity are available, outside host-registration locks and before the application advertises readiness.
3. **Periodic passes at the existing 60-second cadence**. Cover SDK hosts too; the present ticker is documented as started by web persistent mode and CLI driver claim (`coordinator_recheck.go:161–164`), not a universal SDK lifecycle guarantee.
4. **Conditional scope-time recovery:** use dead-host candidates already observed in the scope result. Deduplicate hosts, recover outside the read transaction, then make a fresh scope read.
5. Turn preamble and reset use that same owner-scope operation; ordinary output/status/live-work rendering does not.

**Proposed initial bounds, not measured facts:**

- Startup/periodic maintenance: **one 1-second context budget**, at most **64 root attempts per pass**, with a rotating/keyset cursor.
- Scope path: **one 10-ms total recovery budget**, at most **one candidate root attempt**; timeout/failure remains conservatively live and queues background maintenance.
- No added global recovery enumeration on a no-dead-candidate read.
- One maintenance operation in flight per App; no goroutine per candidate.

**Why W1 is necessary:** current helpers are not bounded by root count. `BatchSweepDeadHosts` lists all recovery roots, then `BatchRecoverDeadHost` lists them again for each host (`batch_recovery.go:37–55,132–153`). Wrapping this in a timeout alone does not make the enumeration or allocation bounded. Add an indexed candidate page and an exact-root recovery operation; retain existing public helpers for their current tests.

**Adversarial qualification:** a 10-ms context is not proof of a 10-ms end-to-end bound on every OS probe or SQLite interaction. CI must measure it. Also, a bounded scope pass cannot guarantee “every crashed root terminal before this call returns” for arbitrarily large scopes. The safe answer is still **live**, followed by eventual recovery—not falsely idle.

The stated D11 budget needs correction: the opened authority specifies **added p95 latency ≤15 ms per ordinary sequential call**, plus **eight-session throughput ≥80%**, under writer contention—not an absolute direct-view p95 of 15 ms (`docs/plans/2026-10-12-job-batches-rush-integration.md:63`; accepted-budget clarification at `docs/research/2026-10-15-r12h-fresh-store-and-sdk-provider.md:278–298`). Preserve both that gate and any separately imposed direct-view measurement; do not silently substitute one for the other.

### 1d. Multi-process arbitration and presentation

**OBSERVED:** the arbiter is the root transaction’s first writer operation, not a process mutex:

```sql
UPDATE batch_nodes SET root_sequence = root_sequence
...
RETURNING *;
```

`internal/db/sql/batch_recovery.sql:11–16` explicitly obtains the writer **before snapshot reads**. A second recoverer rereads the committed terminal state and produces no second interruption/notice (`batch_recovery.go:67–100`). The eight-recoverer test encodes totals of five interrupted nodes, one notice, one control answer, then zero effects on repetition (`batch_recovery_test.go:56–103`).

**INFERRED P1 scenario:** process A commits an announced Running root and dies; process B continues serving the shared directory without targeting that root with `batch_stop`. No automatic ON maintenance runs, and every scope read continues counting the root as live. CLI waiting/delegation closure can remain blocked indefinitely. This is concrete, but was not executed here.

Presentation is not uniform:

- Flat web items map stored `State` to `Status`, `Cause` to `Reason`, and use `OutcomeSummary` only as a title fallback (`internal/server/handlers_livework_batch.go:153–181`). Thus `host_lost` is **not automatically the Reason**: recovery preserves Cause.
- `sessions jobs --json` is built from live-root facts (`internal/cmd/sessions_batch_read.go:371–419`); a recovered terminal root may disappear from that live list. Do not promise it will print a historical interrupted row.
- The agent formatter currently marks interruption only for `Cause == "host_closing"` (`internal/agent/agent_notice_pull.go:179–181`), although recovery can preserve an empty or Stop cause. Consequently a `host_lost` interruption can receive generic “finished” wording from `FormatAsyncCompletion` (`coordinator_background.go:50–59`).

**Decision:** W2 must make recovered interruption honestly visible in the recovery-consumption path—at minimum classify `State == "interrupted"`/`OutcomeCode == "host_lost"` as interruption. Preserve the structured state and retained summary; do not manufacture a cause or executor envelope. Include a post-recovery refresh test for server observers, since the recovery transaction itself does not insert a history message.

### Item 1 test oracles

**W1 — store/scheduler primitives**

- Alive, Unknown, own host, and `(Dead, error)` produce **zero writes**.
- Concurrent independent connections recover all four live states exactly once.
- Existing terminal outcomes/notices survive; unannounced subtree and controls follow the existing policy.
- Candidate paging makes progress across repeated passes and does not starve later roots.
- Revert-check in CI: disabling election or the page cursor must fail the relevant assertions.

**W2 — actual production wiring**

- Crash a helper process after durable announced Running work; start/retain a real App in another process; **do not call `BatchSweepDeadHosts` directly from the test**.
- Startup or periodic production maintenance commits Interrupted/`host_lost`; side-effect counter remains one.
- Scope and preamble paths recover a dead candidate, reread, and stop treating it as live.
- Alive/Unknown negative twins remain live.
- Recovery notice is delivered once on an ordinary pull, **wake remains zero**, interruption is not described as successful completion.
- Short scope budget exhaustion remains conservatively live and later background recovery completes.

The existing crash test helper explicitly invokes the sweep itself (`internal/app/app_batch_provider_crash_test.go:249–256`); that is a store acceptance test, not proof of startup/ticker wiring.

## Item 2 — Observation and timeout parity

### 2a. `wake_only` is a contract; implement it

**OBSERVED authority:** design `:2288–2290` says:

> “`wake_only` does not terminate work and never maps to DeadlineExpired.”

It also names `timeout_wake_only` as non-completion debt that can break ANY/ALL sleep (`:2318–2320`).

**OBSERVED user surface:**

- `internal/agent/tools/bash.go:34`: “lets the work keep running and just notifies you.”
- `internal/agent/tools/run_command.go:49`: the same promise.
- `internal/agent/agent_tool.go:30`: the same promise for delegation.
- `internal/agent/tools/timeout_params.go:15–22`: seconds are “from call start”; a bare number means `wake_only`.
- The opened system prompt selects batch controls (`internal/agent/templates/coder.md.tpl:23–26`); it does not withdraw the timeout schema.

**The gap is larger than the report’s framing.** `batchOneItemSpec` constructs `TaskSpec` with only Kind/Payload (`internal/agent/batch_ingress.go:228–244`), while timeout parsing currently lives in legacy `async_tool.go:451–518`. Therefore “the batch engine always cancels on deadline” is true for an actual `EventDeadlineExpired` (`internal/jobbatch/engine.go:88–89`), but does **not** prove direct tool timeout input reaches that deadline at all.

The parity-gap test also does not arm a deadline or advance a clock: it reserves Running rows without timeout fields and immediately pulls (`batch_timeout_wake_parity_test.go:147–179`). It proves absence of a notice in that fixture, not a production elapsed-time failure.

**Decision / mechanism — W3, integrated by W2:**

- Keep engine `TaskSpec.Timeout` as a **termination deadline only**.
- Move the retained timeout parser out of the file H4 deletes; normalize direct and explicit-batch leaf input before Submit.
- Carry `wake_only` duration and absolute call-start due time in the durable Rush payload. Do not put it into `TaskSpec.Timeout`.
- Use **one App-owned check-in timer service**, not per-session coordinator timers or executor cancellation timers. The service writes retained `session_notices`; it never chooses terminal state.
- Transactionally fence owner/root/node/token, nonterminal state, and non-void root before publishing a check-in.
- Persist a one-shot receipt keyed by exact node incarnation and check-in kind, atomically with the notice. This receipt must survive notice delivery/reaction and ordinary notice retention while the node exists.
- Commit notice first, then hint/wake through the existing arbiter.

**Do not reuse completion notice ordinals.** A node has one selected completion notice by design (`design.md:2205–2208`), and the schema permits only terminal notice states (`20261012000001_batch_projection.sql:43–44`). `notice_ordinal` orders selected completion records; it cannot simultaneously represent an earlier nonterminal check-in and the later completion. A unique check-in receipt plus the retained session-notice ID is the minimal separate identity—not a second terminal outbox.

Restart semantics:

- Restarting an observing coordinator while the execution host remains alive must not reset the due time or duplicate a check-in.
- If the **execution host** dies, §9.3 recovery interrupts the task; no timer restores execution. Recover dead work before considering an overdue check-in.
- A restart between notice commit and wake is safe because retained notice debt is level-readable (`batch_reaction.go:151–203`).

Explicit batch item timeout strings already populate `TaskSpec.Timeout` (`batch_model_tools.go:213–220`). Preserve that contract. Reject ambiguous nested tool-timeout versus item-deadline input rather than silently installing two termination timers; design `:2289–2290` allows only one.

**INFERRED P1 scenario:** a detached `bash` or delegated agent requests a five-second `wake_only` check-in, remains active, and its owner sleeps waiting for non-completion debt. No check-in arrives. Equally, direct `terminate_and_wake` may be silently omitted by the current direct-spec construction. These are explicit deadline-contract violations; CI must exercise actual ingress and held execution.

**Oracle:** fake clock + real ingress, accepted execution held beyond due time: one check-in, no Stop, live node, unchanged admission; repeated scans/restarts produce no duplicate; later completion still yields its one selected completion. Termination twin must latch Timeout and wait for confirmed termination. Replace the current “gap expected” test rather than retaining it as successful acceptance.

### 2b. Supervision check-ins already count as batch debt

The proposed defect is **refuted by the current implementation**.

**OBSERVED chain:**

1. `supervision.go:421–425` inserts a wake-enabled retained notice before `wakeSession`.
2. `coordinator_drain_policy.go:156–158` calls `SelectedDebtExists`.
3. `batch_cutover_facts_node.go:40–49` calls `BatchReactionDebtExists`.
4. `internal/session/batch_reaction.go:151–170` reads **both** nodes and retained notices:

```go
notices, err := q.BatchOutboxDebtNotices(ctx, owner)
...
snap.Notices = append(snap.Notices, ...)
```

5. `:199–203` returns `!snap.Empty()`.
6. `internal/db/sql/batch_outbox.sql:50–52` includes every wake-enabled, unreacted pending/done session notice—no exclusion of supervision.
7. Arbiter facts also include notice debt (`batch_cutover_facts.go:496–519`).

**Decision:** neither suggested production fix is needed. In particular, **do not bypass debt/arbiter gates by “arming the wake directly.”**

There are legitimate refusals: supervision is completion-class for SleepAll (`batch_cutover_facts.go:507–516`), and the pull may void a check-in after scope closure (`batch_cutover_facts_node.go:480–531`). An assertion “every tick must invoke Run” would be wrong.

**W5 oracle:** Running batch root, idle eligible owner, no node completion debt, one pending supervision notice: debt true, arbiter permits the eligible case, one pulled check-in. Negative twins: closed scope voids, SleepAll/policy gates behave as specified, delivery failure preserves pending debt. This guards the already-implemented mechanism; no reproducible P1 was established.

### 2c. Timeout duration and error classification

**OBSERVED:** `BatchNoticeRow` omits duration (`internal/session/batch_pull.go:132–138`), although the scanned node already has `TimeoutNs` (`:87`). The agent converter sets `TimedOut` but not `TimeoutSeconds` and excludes timeout from `IsError` (`agent_notice_pull.go:169–178`). The formatter therefore prints:

```go
"timed out after %ds ..."
```

with zero (`coordinator_background.go:38–40`).

**But:** that formatter checks `TimedOut` before `IsError`; changing `IsError` does not change that timeout text. Moreover, synchronous fallback responses already classify timed-out state as error (`batch_install_adapters.go:318–320`). The opened evidence does **not** establish that the model or API currently receives a successful tool response solely because this temporary `AsyncCompletion.IsError` is false.

**Decision:** accurate presentation parity is justified **P2**, not a demonstrated P1. **W4 backlog; do not fix merely to clear legacy tests.** The design promises distinct timeout outcomes (`design.md:63–65`) and “failure never becomes success,” but explicitly declines identical direct-call formatting (`:2284–2286`).

**Minimal future fix:**

- Add a duration field to `BatchNoticeRow`; populate from the selected task’s immutable timeout.
- Set timeout error classification in `buildBatchNoticeMessageParams`.
- For an aggregate/root timeout, do not borrow a random child’s deadline. Render an aggregate timeout without a fictitious duration unless the relevant deadline is unambiguous.
- Do not add a migration just to copy existing `timeout_ns`.

**Oracle:** a nonzero-duration selected task timeout renders that duration; aggregate timeout does not invent “0s”; failed, interrupted, cancelled, timed-out and completed remain distinct. Assert literal expected semantics, not equality to `FormatAsyncCompletion` fed the same missing zero value—the existing test does exactly that at `batch_timeout_wake_parity_test.go:126–127`.

### 2d. Three supervision presentation gaps

The authority explicitly withdraws blanket legacy-contract preservation (`design.md:7–8`). None of these three presentation details appears in §9’s integration acceptance (`:2419–2437`).

#### Silence flagging — accept removal, P3

**OBSERVED:** the batch summary deliberately returns no flagged jobs (`supervision.go:551–555,617`). Legacy silence depends on real last-output evidence and treats unknown as unknown (`supervision_silence.go:37–71`). It is not safe to derive silence from `updated_at`, root creation time, or polling absence.

Also, the proposed generic-output source is not a live last-write buffer: `batch_exec_generic.go:85–95` records content only after Run returns, and `ReadOutput` waits for Done (`:121–136`).

**Decision:** accept removal with legacy escalation. Keep ordinary batch inspection/stop guidance. A future activity feature needs actual shell/argv write timestamps, known/unknown semantics, exact node identity, and an explicit cross-process policy—not a manufactured fact in `BatchCutoverFacts`.

**Oracle:** batch supervision still describes live work and offers `batch_output`/`batch_stop`; it must not falsely flag unknown activity as silent. No claim that silence proves a hang.

#### Repeating the question in supervision — accept removal, P3

**OBSERVED:** batch summary counts live-held questions but does not repeat text (`supervision.go:582–603`). The actual publisher writes:

```go
text := marker + "SUB-AGENT QUESTION: " + n.Question + ...
```

at `batch_install_question.go:35–37`. The durable notice reaches history through the retained pull (`batch_cutover_facts_node.go:441–501`).

**Decision:** preserve question delivery and Inject identity, not duplicate question prose in a later summary. Design `:2347–2348,2433–2434` requires answering the existing held execution.

**Oracle:** the parent sees the exact question and correct existing node identity once through ordinary notice delivery; supervision says it awaits an answer; stale/terminal incarnation does not count. A missing question notice would be a separate P1—not evidence that this duplicate summary line is required.

#### Idle child with own jobs — accept removal, P3

**OBSERVED:** the old line is assembled through legacy child job maps and background counts (`supervision.go:637–671`). The batch summary instead reads descendant delegation facts (`:571–580`). Its fact interface already supports querying another owner’s live work (`batch_cutover_facts.go:59–78`), so “no conceivable batch fact exists” is too strong.

**Decision:** accept loss of this explanatory line; retain actual descendant liveness and scope closure. Do not rebuild legacy job maps to reproduce prose.

**Oracle:** idle child with a live separate-root task keeps delegation/owner scope live; after task settlement the scope closes correctly. The absence of the old sentence is not failure. The authority is root-based liveness, including work awaiting termination (`design.md:2300–2304`).

### 2e. Unavailable facts returning fabricated debt

**OBSERVED:** the fallback is explicitly conservative (`batch_cutover_facts.go:117–123`):

```go
// answers every question conservatively: debt true, live true.
return true, nil
```

It is not unique: `LiveDelegationChildOf` and `DelegationChildEver` also return `(true, nil)` (`:154–159`). However, the exported unavailable-source error is documented as terminal and to be surfaced, never retried (`:111–115`).

**Decision:** deliberate conservatism, with an error-contract inconsistency; **P2 defer**. No demonstrated wrong execution or data loss.

Changing to `(true, errBatchFactsUnavailable)`:

- `drainDecision` returns the error before `drainPolicy` (`coordinator_drain_policy.go:162–165`);
- `wakeSession` adds the session to the recheck set on that error (`coordinator_wake.go:45–50`);
- today’s `(true,nil)` reaches `readTurnFacts`, where `PendingInclusiveDebt` already returns unavailable and fails closed (`batch_cutover_facts.go:130–135,496–499`).

Thus returning the error earlier does not inherently enable an unsafe turn or lose real debt. But **a one-line change does not implement “never retry”**: it can still enter the recheck set. A future fix must distinguish permanent missing binding from transient DB read errors at callers.

**Oracle:** unavailable binding produces no paid Run, no settled debt, no false idle; CLI/API surfaces the unavailable-source error; permanent errors do not spin a retry loop. Production installation is already intended to fail closed on missing resources (`app_batch_install.go:39–41`).

## Work packages, ownership, and order

All agents use isolated worktrees. **Compilation, formatting checks, lint and tests execute only in GitHub CI. No local execution is proposed.**

### Disjoint file ownership

| Package | Scope and exclusive file set |
|---|---|
| **W1 — bounded recovery core** | `internal/session/batch_recovery.go`; new `internal/session/batch_recovery_candidates.go`; new focused recovery-budget tests. Keep canonical generated recovery SQL unchanged; use a separately named Go-side candidate query consistent with existing repository practice. No App/agent files. |
| **W2 — production wiring and recovery observation** | `internal/app/app_batch_install.go`, `internal/app/app_lifecycle.go`; new `internal/app/app_batch_maintenance.go`; `internal/agent/coordinator_reaction_source.go`, `internal/agent/cli_active_work.go`, `internal/agent/agent_turn.go`, `internal/agent/batch_install_adapters.go`; new agent recovery dependency/helper; the batch reset entry in `internal/cmd/sessions_batch_read.go` or its existing batch-reset helper after locating that entry; `internal/agent/agent_notice_pull.go` **interruption classification only**; dedicated new production-recovery tests. W2 alone owns these integration hooks. |
| **W3 — timeout-intent/check-in mechanism** | `internal/agent/batch_ingress.go`, `internal/agent/batch_model_tools.go`, `internal/agent/batch_exec.go`; new retained timeout parser/service files; `internal/app/app_batch_runtime.go` for service ownership; new `internal/session/batch_timeout_checkin.go`, a dedicated additive migration for durable one-shot receipts, and new timeout tests; `internal/agent/batch_timeout_wake_parity_test.go`. No W2 installation/adapter files. |
| **W4 — timeout formatting, deferred** | `internal/session/batch_pull.go`; future change to `internal/agent/agent_notice_pull.go`; new formatting tests. **Not concurrent with W2:** this is a later backlog package and takes ownership only after W2 is closed. |
| **W5 — consultation regression oracles/backlog record** | New tests-only files for retained supervision debt and batch supervision presentation; one new focused backlog/design-decision document. No production file changes, no edits to existing broad test files. |

W2 is the largest package: keep it to wiring small calls into existing functions, not implementing the recovery algorithm or timer. Its new helper owns shared integration logic. If that exceeds one agent’s scope, split its **new tests** into a successor package rather than giving two agents overlapping App files.

### File-size check

Opened numbered EOFs establish these current sizes for principal production files proposed to grow:

| File | Lines |
|---|---:|
| `internal/session/batch_recovery.go` | 162 |
| `internal/app/app_batch_install.go` | 227 |
| `internal/app/app_lifecycle.go` | 471 |
| `internal/app/app_batch_runtime.go` | 248 |
| `internal/agent/coordinator_reaction_source.go` | 340 |
| `internal/agent/cli_active_work.go` | 130 |
| `internal/agent/agent_turn.go` | **960** |
| `internal/agent/batch_install_adapters.go` | 792 |
| `internal/agent/batch_ingress.go` | 284 |
| `internal/agent/batch_model_tools.go` | 228 |
| `internal/agent/batch_exec.go` | 242 |
| `internal/agent/agent_notice_pull.go` | 194 |
| `internal/session/batch_pull.go` | 217 |
| `internal/cmd/sessions_batch_read.go` | 462 |

**Hard constraint:** `agent_turn.go` gets only a small delegated helper call, not recovery logic. New files remain below 1000 lines. CI must check the final diff’s actual line counts; current headroom is not permission to cross the limit.

### Dependency graph and parallelism

```text
W1 bounded recovery ───────────────┐
                                  ├── W2 production composition ── H4
W3 timeout/check-in mechanism ─────┘
W5 regression oracles/decisions ────── CI acceptance ─────────────── H4

W2 complete ── W4 formatting backlog, only when separately authorized
```

**Proposed order:**

1. Freeze W1/W3 internal dependency signatures and the explicit recovery `wake=0` decision.
2. Run **W1, W3, W5 in parallel**—three agents.
3. W2 may prepare its disjoint wiring concurrently, but integrates only against completed W1/W3 contracts. Maximum **four agents**, not five merely because five are allowed.
4. GitHub CI validates the composed production path before H4.
5. H4 squashes the accepted schema into `rush_store_v1.db` and deletes legacy authority only after those gates. W3’s receipt schema must be included in that one embedded fresh-store migration.
6. Record P2/P3 items and end the review series when no reproducible P0/P1 remains; do not schedule repeated reviews to eliminate the presentation backlog.

### What only CI or the operator can verify

- **CI:** actual Windows/Unix OS-lock behavior; multi-process recovery races; production startup/periodic wiring; crash and commit-fault exits; no duplicate side effects; timer-versus-settlement races; schema squashing; dropped-table operation; final line-count/build/static-check gates.
- **CI performance:** D11 with equivalent production baseline/candidate paths, populated stores, writer contention and eight sessions. Neither source inspection nor a timeout constant establishes the latency bound.
- **CI/browser operator:** whether recovered work refreshes in the real web hierarchy, interruption labels are honest, and reload does not resurrect live work or stale question controls. Browser acceptance is explicitly required by design `:2437–2438`.
- **Operator:** any decision to change recovery from non-waking interruption to autonomous paid resumption; any change to accepted timeout ambiguity policy or performance acceptance. Those are not safe to smuggle into “parity.”

**Files changed: none. Tests run: none. Notable:** supervision debt already includes retained notices; recovery already exists on dead control targets; recovery currently writes `wake=0`; and the present timeout-gap test does not exercise a real deadline.
