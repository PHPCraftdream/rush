# H4 parity decisions — observation/timeout items (2026-10-19)

Source: read-only design review docs/reviews/2026-10-19-h4-prerequisites-review.md (H4 prerequisites;
referenced below as review:LINE). Worktree branch: `jb/h4w5`. One row per
review item: decision, priority, rationale, guarding test.

1. **`wake_only` check-in contract** — IMPLEMENT, **P1**, package W3.
   Rationale: review:165-207 — a direct tool timeout input never reaches
   TaskSpec.Timeout via batchOneItemSpec (batch_ingress.go:228-244); the fix is an
   App-owned check-in timer service that writes retained session notices with a
   one-shot receipt keyed by the exact node incarnation, while the engine's
   TaskSpec.Timeout stays termination-only.
   Guard: W3's own oracle (fake clock + real ingress; exactly one check-in, no
   duplicate across restarts).

2. **Supervision check-ins count as batch debt** — ALREADY IMPLEMENTED, no change.
   Rationale: review:209-234 — the full chain already exists: supervision.go:421-425
   → coordinator_drain_policy.go:156-158 → batch_cutover_facts_node.go:40-49 →
   internal/session/batch_reaction.go:151-203 → internal/db/sql/batch_outbox.sql:50-52
   (BatchOutboxDebtNotices is kind-agnostic). The review's two proposed production
   fixes are refuted; do NOT arm wakes past the debt/arbiter gates.
   Guard: `internal/agent/batch_supervision_debt_test.go` (tests
   TestBatchSupervisionNoticeDebt_*).

3. **Timeout duration + error classification** — DEFER, **P2**, W4 backlog after W2
   closes. Rationale: review:236-257 — BatchNoticeRow omits the duration even
   though TimeoutNs is scanned (batch_pull.go:87,132-138), and
   buildBatchNoticeMessageParams sets TimedOut without TimeoutSeconds so the
   formatter prints "after 0s" (coordinator_background.go:38-40). Accurate duration,
   error classification and aggregate timeouts must never invent a duration by
   borrowing a child's deadline; no schema migration just to copy timeout_ns.
   Guard: `internal/agent/batch_notice_presentation_test.go`
   (TestBatchNoticeFormatting_CharacterizesCurrentTerminalWording — the literal
   "after 0s" is the assertion W4 flips).

4. **Silence flagging in batch supervision** — ACCEPT REMOVAL, **P3**.
   Rationale: review:263-271 — legacy silence needs real last-output evidence
   with known/unknown semantics; batch output is recorded only after Run returns
   (batch_exec_generic.go:85-95), so no live last-write seam exists. A future feature
   needs real shell write timestamps and an explicit cross-process policy.
   Guard: `internal/agent/batch_notice_presentation_test.go`
   (TestBatchSupervisionSummaryBatch_NoFalseSilenceFlagging).

5. **Repeating question text in supervision summary** — ACCEPT REMOVAL, **P3**.
   Rationale: review:273-285 — the durable child_question notice already carries
   the exact question and marker identity (batch_install_question.go:35-37); the
   design (design doc :2347-2348) requires answering the existing held execution, not
   duplicating prose.
   Guard: `internal/agent/batch_notice_presentation_test.go`
   (TestBatchQuestionNotice_ExactDeliveryAndAwaitingCount).

6. **Idle-child own-jobs explanatory line** — ACCEPT REMOVAL, **P3**.
   Rationale: review:287-293 — descendant liveness and scope closure are
   preserved via batch facts (batch_cutover_facts.go:59-78); do not rebuild legacy
   job maps for prose; root-based liveness per design doc :2300-2304.
   Guard: `internal/agent/batch_notice_presentation_test.go`
   (TestBatchIdleChildSeparateRootKeepsScopeLiveThenCloses).

7. **unknownBatchCutoverFacts conservative (true, nil)** — DEFER, **P2**.
   Rationale: review:295-316 — the conservatism is deliberate
   (batch_cutover_facts.go:117-123, also LiveDelegationChildOf/DelegationChildEver
   :154-159). A one-line change to (true, errBatchFactsUnavailable) does NOT implement
   "never retry": wakeSession's recheck-set semantics (coordinator_wake.go:45-50)
   still re-read. A future fix must distinguish a permanent missing binding from a
   transient DB read error at the callers.
   Guard: `internal/agent/batch_supervision_debt_test.go`
   (TestBatchCutoverUnknownFacts_ConservativeNoRunNoIdle).

8. **Heartbeat label under the batch runtime** — ACCEPT, no change.
   The wait heartbeat names the task kind (`shell` / `argv`) and the owning
   host PID instead of the tool name (`bash` / `run_command`), because the batch
   task row stores the kind, not the tool name (package T2 restored the PID).
   Storing the tool name would need a schema change; not worth it.
   Guard: the T2 heartbeat tests (`app_run_wait_heartbeat_test.go`).

9. **Supervision arming under the batch runtime** — RESTORED (package SV, PR #171).
   Rationale: the legacy arming points did not exist under the flag; the orchestrator
   restored them at the batch started boundary. A paused supervision entry whose
   work drains without a later notice, cancel or new work keeps its registry
   entry (bounded by sessions; P2, recorded in PR #171).
   Guard: `internal/agent/batch_supervision_arming_test.go`.

## Operator sign-off (recorded)

Signed off by the operator on 2026-10-21 ("делай как ты рекомендуешь"), accepting
the recommendations as written:

- Item 4 — silence flagging in batch supervision: removal accepted (P3).
- Item 5 — repeating question text in the supervision summary: removal accepted (P3).
- Item 6 — idle-child own-jobs explanatory line: removal accepted (P3).
- Item 8 — heartbeat shows the task kind, not the tool name: accepted.

Items 3 and 7 stay deferred P2 (not mandatory for H4 without a new reproducible P1).

## H4 rollout decisions (same sign-off, 2026-10-21)

Source: docs/reviews/2026-10-19-h4-prerequisites-review.md and the H4 execution
plan (reviewer consultation #7). These bind the H4 documentation and tests.

1. **Data semantics.** The importer carries durable history, settings, queues and
   schedules; it discards old and preparation-era execution and control state
   (`async_jobs`, batch/control/checkpoint/check-in rows, hosts, drivers). H4 does
   not continue in-flight jobs across the upgrade.
2. **Quiescence.** Stop every old rush process and any automatic restart for the
   whole import; there is no registration fence against a process that starts
   during it. Mixed-generation use of shared locks, config and attachments is
   unsupported.
3. **Owner-stop fences from before the `owner_stop:` namespace (package SF).**
   No hot continuity: stop all preparation hosts before upgrading; bare-UUID
   control rows are never relabelled as owner-stop.
4. **Existing `rush_store_v1.db`.** Never overwritten and never re-imported. To
   re-import, the operator moves the file (and its sidecars) aside by hand.
5. **Imported queued work.** Running operator tasks and leased continuations are
   reclaimed and retried; a retry can repeat an external side effect. The importer
   is not exactly-once for external effects. State this in the rollout notes.
6. **Historical web completion display.** Keep it as read-only compatibility for
   imported transcripts; do not remove the rendering together with the legacy rows.
7. **SDK execution fence.** Keep the interim reserved `session_run_queue` row
   through H4 (it is not a legacy execution authority); the move to a
   `sessions.execution_requirement` column is a separate later change.
8. **Versions.** H4 does not bump any version, tag or release. Incompatibility and
   rollback are documented without touching version numbers.
