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

## Operator sign-off needed

- Item 4 — silence flagging in batch supervision (accept removal, P3).
- Item 5 — repeating question text in the supervision summary (accept removal, P3).
- Item 6 — idle-child own-jobs explanatory line (accept removal, P3).
