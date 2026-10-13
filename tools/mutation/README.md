# Temporary mutation proof

Push `mut/<name>` from the **exact integration commit** being proved, with the
workflow, runner and manifests present. Only `mut/**` pushes trigger Actions;
no main/PR/dispatch/schedule trigger. Permissions are read-only, concurrency does
not cancel executing runs; replaced pending runs remain unproved. Record the
checkout SHA: results do not prove later commits.

Inspect Actions step summaries and download per-shard results and JSON/stderr
artifacts, including failed jobs. Upload uses the operator-verified v4.6.2 pin
`actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02`.
Verification provenance is the operator's confirmation, not a local action audit.

## Schema and execution

UTF-8 JSON: `schema_version: 1`, `task`, `mutations`. Each record has `id`,
`file`, `file_sha256`, `needle`, `replacement`, `package`, `test`, `purpose`,
`expected_assertion`. Paths are repository-relative POSIX; hashes are SHA-256 of
LF HEAD blob bytes obtained through read-only `git show`, never CRLF disk hashes.
Go packages are concrete `internal/...` paths, with one literal top-level
`Test...` name. Additional advertised tests are purpose notes, not extra proofs.
Empty expected assertions explicitly mean no distinctive kill is determinable;
they intentionally fail validation. Never invent a test or generic `FAIL`.

The runner copies tracked HEAD source **once per shard**, outside the checkout,
then applies mutations sequentially with in-memory original bytes. Baselines are
deduplicated by package/test inside a shard. Restoration verifies exact bytes
and SHA; there is **no restored-test rerun**. This is the cost-reduced execution
variant, not the original recommendation's fresh-directory/post-test algorithm.
Subprocesses are externally bounded to 15 minutes including compilation;
timeouts are errors, not kills. Tests use Linux race/JSON/count=1, serial package
and test execution, five-minute Go timeout, no short mode. Package groups are
dealt round-robin when enough packages exist; otherwise mutations are assigned
round-robin. No regeneration for generated SQL Go targets.

Integrity refuses source drift as `EXECUTION_ERROR`: mismatched SHA, modified
normalized working bytes, or non-unique needles must not be repaired by silently
changing the reviewed mutation. Named run/fail events plus the advertised
assertion are required for a kill; assertion output may come from that exact test
or descendants named `TestParent/...` in the same full package. Run/pass/fail
classification still requires the exact named top-level test. Baseline failures,
including command-launch errors, yield `BASELINE_ERROR` for every mutation sharing
that package/test pair. Compilation failures and unrelated test
failures are not proof. Survival, errors and incomplete evidence fail the job.
**Revalidate every manifest against the final integration commit**, then rerun
its proofs; sibling validation is only conversion evidence, not integration
proof. Delete the temporary workflow, runner, manifests and this README before
PR #12 becomes merge-ready for main; retain proof artifacts outside main.

## Conversion inventory

- R7c: 15 records, including distinct rc4a/rc4b. rc10 is retargeted to the
  strengthened `TestBatchAgentHeldQuestionAfterTurnEnded` with expected
  assertion `a question is held after the turn ended` (per the vacuous list);
  its purpose now states the after-turn-ended backstop kills it.
- R10b: all 17 supplied records from `.mutsrc/lists/r10b.mutlist.json`, with
  supplied IDs, needles, replacements, tests, purposes and expected assertions
  unchanged. Only `./internal/agent` is normalized to `internal/agent`; LF HEAD
  blob hashes re-verified against base 75136c49 (no refresh needed). Workflow
  shards are 1/2 and 2/2. m16 remains an uncertain killer:
  initial evaluation can race with root completion and satisfy ANY before a hint.
  A survivor is a test finding, not a runner defect; do not silently strengthen
  the fixture or reclassify survival as proof.
- R10c: 20 records (the full 21-record `.mutsrc/lists/r10c.mutlist.json` minus
  m18, excluded below). SQL-source mutations target equivalent compiled sqlc Go
  query text without regeneration. m13 changes only the emitted announce arm.
  m11 uses the vacuous-list record: the strengthened snapshot test seeds a void
  unreacted woken row, so test/purpose/expected assertion come from there.
  Every needle was re-verified against the regenerated
  `internal/db/batch_history.sql.go` at base 75136c49: all occur exactly once,
  but the regeneration DID change content, so the R10c refresh is real:
  the blob's SHA-256 moved
  `302393160fc38933781577f8332d21a16392c02fe62e2944bd6338bade486683` ->
  `a62789f5efce9716773ffed61da634185f23fb760dba69821cdb7036fa0a4ae6`;
  the m07/m12 needles now end with the closing backtick (`` ` ``) where the
  old ones ended with a bare newline, and m10's needle likewise differs from
  the HEAD-parent version.
- R11B: all 17 emitted scripts, not the generator, re-validated against the
  merged base. The sqlc regeneration changed the needle-bearing blob (see the
  R10c entry above for the exact SHA and needle deltas). m02/m17 use the earlier
  primary-test NoError failure, not their advertised later root-count messages.
- R11E: 7 records from `.mutsrc/lists/r11e.mutlist.json`, packages normalized
  to `internal/app`; one workflow shard (single package, 7 mutations).
- Teardown: 5 records from `.mutsrc/lists/teardown.mutlist.json`, task name
  exactly `teardown`, packages normalized to `internal/agent`; one workflow
  shard (single package, 5 mutations).
- R11C: 11 records from `.mutsrc/lists/r11c.mutlist.json`, validated against
  the read-only sibling worktree (HEAD `a495f69d`). Carries top-level
  `"requires_merge": ["#29"]` (ignored by the validator; excluded from the
  workflow matrix until the merge). M7's test is the top-level parent
  `TestBatchInvocationValidateFailsClosed` because the schema forbids subtest
  paths; the `missing_session` subtest runs under it and descendant output
  still classifies the kill.
- R11D: 12 records from `.mutsrc/lists/r11d.mutlist.json`, validated against
  the read-only sibling worktree (HEAD `bec1de66`). Bare `agent` packages are
  normalized to `internal/agent` and bare `session` to `internal/session`; the
  mid-comment-starting needle occurs exactly once. Carries
  `"requires_merge": ["#29", "R11D PR"]`; excluded from the workflow matrix.
  All 12 author records validate, so none was dropped (the plan anticipated
  10; see the Round4 report).
- R15a: 6 Go records: mut1/mut3/mut4/mut6/mut8 plus mut5 restored using the
  vacuous-list record — the strengthened fixture now differs in ordinal vs
  NodeID order, so the real assertion `nodes must be ordered by ordinal, not
  arrival or NodeID` applies. mut3 retains the inline equivalent
  of its helper/import parser transformation; no target needles were altered.
  (The round plan said 7; the delivered set is 6 — mut2/mut7 remain web-test
  mutations and stay excluded.)

## Known gaps

- R10c **m18** (`m18_reset_scan_outside_transaction`,
  `TestBatchResetVsConcurrentReserveRace`) is **excluded** from the manifest:
  it has an empty `expected_assertion` because its kill is scheduling-dependent
  — the current test has no barrier forcing Reserve into the scan/wipe window
  the mutant opens, so a survivor would be unprovable. Add a barrier, name a
  real assertion, then restore the record.
- **R11C and R11D await merges.** Their targets exist only in the read-only
  sibling worktrees (`jobbatch-r11c` HEAD `a495f69d`, `jobbatch-r11d` HEAD
  `bec1de66`). Both manifests carry a top-level `requires_merge` field
  (R11C: `["#29"]`; R11D: `["#29", "R11D PR"]`) and are **excluded from the
  workflow matrix** until those merges land. Sibling validation is conversion
  evidence only, not integration proof; revalidate against the final
  integration commit and rerun their proofs after merging.
- R10c and R11B previously resided in sibling worktrees; all of their targets
  are now present in this integration worktree and validate at base 75136c49.

### Named-test assertion review

These are source-derived assertion choices, not observed mutation executions.
The previously empty rc10/m11/mut5 assertions are now real (strengthened
tests); only R11B's choices remain source-derived.

| Record | Choice | Exact assertion anchor and reason |
|---|---|---|
| R7c rc10 | `a question is held after the turn ended` | Strengthened `TestBatchAgentHeldQuestionAfterTurnEnded` holds a question across a finished turn; vacuous-list record supplies test and assertion. |
| R10c m11 | `settled unreacted woken nodes are debt; a voided delivery row never counts` | Strengthened snapshot test seeds a void unreacted woken row; vacuous-list record supplies test, purpose and assertion. |
| R11B m02 | `jobbatch: commit failed: error` | `internal/session/batch_commit_test.go` primary NoError at the commit hook precedes the root-count assertion. |
| R11B m17 | `jobbatch: commit failed: error` | The root-less new UUID fails the Add inside `batchProjectRoot`; primary NoError precedes the root-count assertion. |
| R15a mut5 | `nodes must be ordered by ordinal, not arrival or NodeID` | Strengthened fixture orders ordinals differently from NodeID; vacuous-list record supplies the assertion. |

R11B's error substring is anchored in `internal/jobbatch/errors.go:20`
(`ErrCommitFailed = errors.New("jobbatch: commit failed")`) and
`internal/jobbatch/runner_commit.go:88,110,298`: a hook error retains reason
`error`, and `signalFault` formats `%w: %s`. m02 assigns empty root and skips
UUID generation; it does not generate a replacement UUID. m17 changes root_id,
not the deterministic child node_id. `batchStoreTestDB` uses `db.Connect` with
foreign keys ON. No test name is used as an assertion substring.

## Not provable by the named test

- R10c m18 (`m18_reset_scan_outside_transaction`): kill is scheduling-dependent
  (see Known gaps).
- R11E `fault-fence-dropped`: Fenced() short-circuits on the attached Runner's
  closed Faulted channel; no named fixture exercises fence-before-channel-closure.
- R11E `release-writer-fence-check-dropped`: the fault fires inside
  ReleaseHostIdentity, so the fence re-check returns before the mutated line.
- R11E `fenced-channel-check-dropped`: the mutant makes Fenced() true whenever a
  runner is attached, so TestOrdinaryShutdownDrainsBeforeRelease blocks at
  `<-runner.closeEntered` (app_batch_drain_test.go:28) before its require — a
  hang (rc None is EXECUTION_ERROR, not a kill). Dropped; R11E is 7 records.
- R15a mut2/mut7: target actual Node/TypeScript tests in
  `web/src/batchWork.test.ts`, not Go tests.

## Local checks

From the repository root:

```bash
python -I tools/mutation/run.py --selftest
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/R7c.json
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/R10b.json
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/R10c.json
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/R11B.json
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/R11E.json
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/Teardown.json
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/R15a.json
python -I tools/mutation/run.py --validate --root <r11c-sibling> --manifest tools/mutation/R11C.json
python -I tools/mutation/run.py --validate --root <r11d-sibling> --manifest tools/mutation/R11D.json
```

`--selftest` uses canned Python fixtures without invoking Go. `--validate` is
read-only: no Go commands, checkout copies, mutation runs, results, logs,
artifacts or step-summary writes. Exit zero means validation, not mutation
proof. Extra top-level manifest keys such as `requires_merge` are tolerated by
the validator (it reads known keys via `data.get`) and ignored for validation.

## Running a proof

From the exact integration commit to prove (workflow, runner and manifests
present), push a temporary `mut/` branch:

```bash
git push origin <commit>:mut/<name>
```

Only `mut/**` pushes trigger `mutation-check`; there is no schedule or manual
dispatch. Results appear in the Actions **step summary** of each shard job;
also download the per-shard `results.json` and log artifacts, including failed
jobs. **Revalidate every manifest against the final integration commit before
pushing**; sibling or earlier-commit validation is not proof of this commit.
If the commit moved since validation, revalidate and push from the new commit.

## Round5 validation report (current round)

Round5 fix pass: every `expected_assertion` re-checked against the FIRST
failing assertion under the mutant (test sources + production paths read;
testify v1.11.1 confirmed: `Error`/`ErrorContains`/`Error` on nil err print
only `An error is expected but got nil.`; `ErrorIs` on nil err prints
`Expected error with %q in chain but got nil.` incl. the quoted target text;
`ErrorIs` non-nil prints `Target error should be in err chain:` + quoted
target; Equal header `Not equal:`, differing types `%T(%v)`, same type `%#v`;
`Zero` -> `Should be zero, but was`; bare `NoError` -> `Received unexpected
error` + the error text; `Eventually` -> `Condition never satisfied` + msg
args). rc None (timeout/hang) is EXECUTION_ERROR, never KILLED.

| Task | Root | Records | OK | Invalid | Needle | SHA | Diagnostics | Exit |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| R7c | base | 15 | 15 | 0 | 0 | 0 | 0 | 0 |
| R10b | base | 13 | 13 | 0 | 0 | 0 | 0 | 0 |
| R10c | base | 20 | 20 | 0 | 0 | 0 | 0 | 0 |
| R11B | base | 17 | 17 | 0 | 0 | 0 | 0 | 0 |
| R15a | base | 6 | 6 | 0 | 0 | 0 | 0 | 0 |
| R11E | base | 7 | 7 | 0 | 0 | 0 | 0 | 0 |
| Teardown | base | 5 | 5 | 0 | 0 | 0 | 0 | 0 |
| R11C | r11c sibling | 10 | 10 | 0 | 0 | 0 | 0 | 0 |
| R11D | r11d sibling | 12 | 12 | 0 | 0 | 0 | 0 | 0 |

Selftest PASS (0 failures); `actionlint .github/workflows/mutation-check.yml`
exit 0.

Changed/dropped records (id | old -> new | DROPPED):
- R11E `fault-exit-code-zero`: `never-returning commit hook must exit the
  process with 70` -> `the helper must exit non-zero`
  (app_batch_subprocess_test.go:102).
- R11E `host-release-error-inverted`: `host busy` ->
  `An error is expected but got nil.` (ErrorContains on nil err).
- R11E `fenced-channel-check-dropped`: DROPPED (hang, see above).
- R11C `M1-singleflight-drop`: `matching retries execute exactly once` ->
  `invalid memory address or nil pointer dereference` — the mutant registers
  the entry under a mangled key, so `rt.entryFor(...)` returns nil and the
  test panics at the `.joined` dereference (batch_ingress_test.go:250) before
  any assertion.
- R11C `M2-mismatch-accept`: DROPPED — accepting the mismatched hash makes the
  retry join the in-flight entry and block on the never-closed fake gate;
  TestInvokeMismatchedHashFails deadlocks (EXECUTION_ERROR, not a kill).
- R11C `M3-controls-allowed`: `control-plane tools are never leaves` ->
  `Should be false` — the classifier at admission still refuses with
  errBatchControlPlaneLeaf, so the first ErrorIs passes and
  `require.False(errors.Is(err, errBatchSubmitBeforeInvalid))`
  (batch_ingress_test.go:347) fails first.
- R11C `M5-admission-swallow`: -> `An error is expected but got nil.` (the
  ErrorIs at batch_runtime_test.go:214 sees a nil error).
- R11C `M6-notwired-fallthrough`: -> `Target error should be in err chain:`
  (the fixture leaves Delivery AND Runner nil, so bypassing the Delivery guard
  reaches the Runner guard; ErrorIs at batch_runtime_test.go:89 fails).
- R11C `M7-identity-session`: -> `An error is expected but got nil.`
  (require.Error at batch_invocation_test.go:66).
- R11D `r11d-inline-duplicate-delivery`: `exactly one completion delivery` ->
  `the started loser returns the envelope read-only` — the loser's duplicate
  inline election is recorded by the fake, so the loop assertions pass and the
  inlineCalls equality (batch_delivery_test.go:246) fails first.
- R11D `r11d-sync-cancel-skips-stop`: -> `exactly one Stop on caller
  cancellation` (ctx.Err() ErrorIs at batch_cancel_test.go:177 still passes).
- R11D `r11d-inline-no-retry-on-failed-ack`: -> `An error is expected but got
  nil.` (ErrorIs at batch_delivery_test.go:202).
- R11D `r11d-escape-window-defaulted`: -> the ErrorIs nil-err literal quoted
  in the table (the 30s default still fails closed, after its grace).
- R10b `m04_any_waits_all`, `m05_each_child_invisible`,
  `m07_skip_initial_evaluation`, `m16_ignore_hint_evaluation`: DROPPED — each
  leaves the worker waiting with no further hints (poll 1h, worker cap 40min),
  so the named test hangs (EXECUTION_ERROR, not a kill).
- R10c `m09_snapshot_probe_bypassed`: `expected: 2` -> `Not equal:` (first
  failure is `require.Equal(HostStatusDead, dead.Host)`,
  batch_history_snapshot_test.go:60).
- R10c `m12_dispose_second_call_voids_again`: first-call count still passes;
  `require.Zero(t, again, "disposal happens exactly once")`
  (batch_consumers_test.go:204) fails first.
- R10c `m15/m16/m17_reset_*`: -> the ErrorIs nil-err literal quoted in the
  table (nil err; ErrorIs at batch_history_reset_test.go:118).
- R15a `mut6_host_unknown_dead`: `expected: "unknown"` -> `Should be false` —
  a dead verdict counts as complete, so `require.False(snap.Complete)`
  (handlers_batch_test.go:168) fails first.
- R7c `rc6_stop_ack_settles`: -> `scope reported while descendants are still
  live after the ack` (the skipped watch loop still marks closed; the report
  arrives at the second noReport, batch_exec_agent_test.go:445/192).

R11B m02/m17 verified: the hook fault reason is `"error"`
(runner_commit.go:293 default), the runner fault is
`jobbatch: commit failed: error`, and the first failing assertion is a bare
`require.NoError` (batch_commit_test.go:119 / :330), which prints
`Received unexpected error` followed by that text. R11D
`r11d-detached-no-started-election` verified: bare Equal on int64 prints
same-type `%#v`, so `expected: 1` IS printed (batch_delivery_test.go:259).

## Assertion rationale

One row per record in ALL manifests: the expected_assertion literal and the
test file:line where the first failing assertion prints it.

| Manifest | id | expected_assertion | printed at |
|---|---|---|---|
| R7c | rc1_scope_before_close | scope reported while the turn loop runs | batch_exec_agent_test.go:192/225 |
| R7c | rc2_report_twice | duplicate report: | batch_exec_agent_test.go:243 |
| R7c | rc3_incomplete_closed | scope reported while the read is unknown | batch_exec_agent_test.go:192/258 |
| R7c | rc4a_inject_after_stop_scope | batch agent: incarnation is stopped | batch_exec_agent_test.go:642 (ErrorIs quoted target) |
| R7c | rc4b_inject_after_stop_exec | batch agent: incarnation is stopped | batch_exec_agent_test.go:384 (ErrorIs quoted target) |
| R7c | rc5_stale_generation | batch agent: stale incarnation | batch_exec_agent_test.go:401 (ErrorIs quoted target) |
| R7c | rc6_stop_ack_settles | scope reported while descendants are still live after the ack | batch_exec_agent_test.go:445 via :192 |
| R7c | rc7_wrong_depth | batch agent: no held question at that depth | batch_exec_agent_test.go:338 (ErrorIs nil-err message) |
| R7c | rc8_dispose_no_barrier | context deadline exceeded | batch_exec_agent_test.go:470 (ErrorIs nil-err message) |
| R7c | rc9_leak_driver_waiter | context deadline exceeded | batch_exec_agent_test.go:507 (NoError) or :470 |
| R7c | rc11_panic_unclassified | *agent.batchStartError | batch_exec_agent_test.go:545 (ErrorAs) |
| R7c | rc12_iserror_completed | expected: "failed" | batch_exec_agent_test.go:570 |
| R7c | rc13_watch_ignores_done | scope reported while the turn loop runs | batch_exec_agent_test.go:192/225 |
| R7c | rc14_stop_no_cancel | Condition never satisfied | batch_exec_agent_test.go:444 |
| R7c | rc10_held_not_live | a question is held after the turn ended | batch_exec_agent_test.go:192/314 |
| R10b | m01_incomplete_finished | Should be true | batch_await_test.go:229 |
| R10b | m02_capture_phantom_root | result units are captured roots only | batch_await_test.go:304 (NotContains prints the unmet message) |
| R10b | m03_all_child_satisfied | a settled child alone must not finish ALL under NotifyEach | batch_await_test.go:333 |
| R10b | m06_disappeared_success | a disappeared root is never finished | batch_await_test.go:384 |
| R10b | m08_pending_not_live | pending | batch_await_test.go:476 (True msgAndArgs) |
| R10b | m09_cancelling_not_live | cancelling | batch_await_test.go:476 (True msgAndArgs) |
| R10b | m10_stop_intent_completion | stop intent is not a completion | batch_await_test.go:495 |
| R10b | m11_ignore_debt | Should be true | batch_await_test.go:461 |
| R10b | m12_timeout_error | timeout reports remaining without an error | batch_await_test.go:241 (NoError) |
| R10b | m13_incomplete_zero_refusal | incomplete capture with zero refs must not refuse | batch_await_test.go:210 (NoError) |
| R10b | m14_workopen_forced | an open once wake alone must not open WorkOpen | batch_await_test.go:561 |
| R10b | m15_disable_maxwait_bounds | max_wait=10 | batch_await_test.go:256 (ErrorContains msgAndArgs) |
| R10b | m17_bypass_all_orchestrator_fix | a settled child alone must not finish ALL under NotifyEach | batch_await_test.go:333 |
| R10c | m01_reconcile_drops_done | expected: int(1) | batch_consumers_test.go:94 |
| R10c | m02_reconcile_drops_pending | expected: int(1) | batch_consumers_test.go:94 |
| R10c | m03_reconcile_missing | expected: int(1) | batch_consumers_test.go:94 |
| R10c | m04_reconcile_keeps_counters | Should be zero, but was | batch_consumers_test.go:98 |
| R10c | m05_owner_delete_leaves_controls | expected: int(1) | batch_consumers_test.go:130 |
| R10c | m06_owner_delete_refuses_live | deletion refuses nothing, live root included | batch_consumers_test.go:129 (NoError) |
| R10c | m07_dispose_guard_missing | only pending/done unreacted woken rows dispose | batch_consumers_test.go:178 |
| R10c | m08_dispose_wake_not_zeroed | wake zeroed so debt readers cannot double-count | batch_consumers_test.go:187 |
| R10c | m09_snapshot_probe_bypassed | Not equal: | batch_history_snapshot_test.go:60 |
| R10c | m10_roots_drops_terminal | should have 2 item(s), but has 1 | batch_history_snapshot_test.go:110 |
| R10c | m11_snapshot_debt_counts_void | settled unreacted woken nodes are debt; a voided delivery row never counts | batch_history_snapshot_test.go:68 |
| R10c | m12_dispose_second_call_voids_again | disposal happens exactly once | batch_consumers_test.go:204 |
| R10c | m13_live_rerun_unreachable | both live roots are routed for stop | batch_history_rerun_test.go:333 |
| R10c | m14_repend_keeps_dead_ref | batch store: projection identity or sequence conflict | batch_history_rerun_test.go:275 (NoError prints the fault text) |
| R10c | m15_reset_omits_pending | Expected error with "batch reset: the session still owns a nonterminal batch root" in chain but got nil. | batch_history_reset_test.go:118 |
| R10c | m16_reset_omits_cancelling | Expected error with "batch reset: the session still owns a nonterminal batch root" in chain but got nil. | batch_history_reset_test.go:118 |
| R10c | m17_reset_pending_control_is_terminal | Expected error with "batch reset: the session still owns a nonterminal batch root" in chain but got nil. | batch_history_reset_test.go:118 |
| R10c | m19_rerun_legacy_lookup_unguarded | a reused call id must not void the acked new incarnation: | batch_history_rerun_test.go:201 |
| R10c | m20_reserve_reuses_terminal_root | a rerun gets a fresh incarnation | batch_history_rerun_test.go:153 |
| R10c | m21_rerun_stop_routing_by_call_id | a reused call id must not void the acked new incarnation: | batch_history_rerun_test.go:201 |
| R11B | m01_reserve_nonatomic | tree insert rolled back when the initial-view projection fails | batch_commit_test.go:265 |
| R11B | m02_ignore_root_id | jobbatch: commit failed: error | batch_commit_test.go:119 (bare NoError prints the fault text) |
| R11B | m03_drop_host_compare | batch store: live root owned by another host | batch_commit_close_test.go:150 (ErrorIs quoted target) |
| R11B | m04_drop_incarnation_compare | batch store: live root held by another runner incarnation | batch_commit_test.go:486 (ErrorIs quoted target) |
| R11B | m05_accept_overflow | batch commit: uint64 value exceeds sql int64 range | batch_commit_test.go:521 (ErrorIs quoted target) |
| R11B | m06_skip_envelope | Input ('') needs to be valid json | batch_commit_test.go:383 |
| R11B | m07_notice_structural_ordinal | expected: "0" | batch_commit_test.go:357 |
| R11B | m08_reconstruct_notices | ordinal is the slice position, not the structural ordinal | batch_commit_test.go:351 |
| R11B | m09_closing_wake_one | host-closing notices never wake | batch_commit_close_test.go:90 |
| R11B | m10_rewrite_latched_cause | already-latched cause not rewritten by host closing | batch_commit_close_test.go:85 |
| R11B | m11_finalize_settlements | interruption facts persisted | batch_commit_close_test.go:88 |
| R11B | m12_retry_commit | hook error returned as-is, not wrapped | batch_commit_close_test.go:240 |
| R11B | m13_wrap_hook_error | hook error returned as-is, not wrapped | batch_commit_close_test.go:240 |
| R11B | m14_drop_coalesce_passthrough | COALESCE must preserve the stored child_session_id | batch_commit_close_test.go:312 |
| R11B | m15_drop_notice_selected_guard | Expected error with "batch store: projection identity or sequence conflict" in chain but got nil. | batch_commit_close_test.go:47 |
| R11B | m16_drop_projection_sequence_fence | Expected error with "batch store: projection identity or sequence conflict" in chain but got nil. | batch_commit_test.go:502 |
| R11B | m17_add_second_root | jobbatch: commit failed: error | batch_commit_test.go:330 (bare NoError prints the fault text) |
| R15a | mut1_incomplete_treated_complete | a failed fetch must make the scope incomplete | handlers_batch_test.go:127 |
| R15a | mut3_text_parsing_reintroduced | OutcomeCode: | handlers_batch_test.go:173 (Equal struct diff) |
| R15a | mut4_flag_ignored | Should be false | handlers_batch_test.go:186 |
| R15a | mut6_host_unknown_dead | Should be false | handlers_batch_test.go:168 |
| R15a | mut8_route_flag_off | unknown command: get_session_batch_work | handlers_batch_test.go:212 (Contains prints both strings) |
| R15a | mut5_ordinal_order | nodes must be ordered by ordinal, not arrival or NodeID | handlers_batch_test.go:158 |
| R11E | fault-exit-code-zero | the helper must exit non-zero | app_batch_subprocess_test.go:102 |
| R11E | fault-watcher-ignores-runner | exit must be called once | app_batch_fault_test.go:30 (Eventually msg) |
| R11E | host-release-error-inverted | An error is expected but got nil. | app_batch_drain_test.go:109 |
| R11E | close-timeout-releases | Close timeout reports Forced | app_batch_drain_test.go:55 |
| R11E | writer-release-dropped | drain precedes release; host identity precedes writer | app_batch_drain_test.go:36 |
| R11E | fault-during-drain-not-forced | fault during drain forces the result | app_batch_fault_test.go:106 |
| R11E | setrunner-wake-dropped | watcher must wake on SetRunner and exit once | app_batch_watch_test.go:30 (Eventually msg) |
| Teardown | release-join-omitted | release join reached after-waits seam before callback exit | release_callback_join_test.go:97 |
| Teardown | callback-admission-reopened | refused callback must not enter the seam | release_callback_join_test.go:144 |
| Teardown | shutdown-context-not-cancelled | cooperative callback should join gracefully | release_callback_join_test.go:167 |
| Teardown | notify-join-omitted | notify join reached after-waits seam before callback exit | release_callback_join_test.go:97 |
| Teardown | forced-verdict-suppressed | unjoined release callback must report stillBusy | release_callback_join_test.go:129 |
| R11C | M1-singleflight-drop | invalid memory address or nil pointer dereference | batch_ingress_test.go:250 (panic, not an assertion) |
| R11C | M3-controls-allowed | Should be false | batch_ingress_test.go:347 |
| R11C | M4-attach-resubmit | an attached retry never submits | batch_ingress_test.go:323 |
| R11C | M5-admission-swallow | An error is expected but got nil. | batch_runtime_test.go:214 |
| R11C | M6-notwired-fallthrough | Target error should be in err chain: | batch_runtime_test.go:89 |
| R11C | M7-identity-session | An error is expected but got nil. | batch_invocation_test.go:66 |
| R11C | M8-policy-scope-drop | Not equal: | batch_runtime_test.go:151 |
| R11C | M9-construct-probe | construction must not probe | batch_runtime_test.go:67 |
| R11C | M10-finish-no-evict | a failed call leaves the singleflight map | batch_ingress_test.go:416 |
| R11C | M11-waiter-poison-replay | Received unexpected error | batch_ingress_test.go:506 (NoError) |
| R11D | r11d-inline-duplicate-delivery | the started loser returns the envelope read-only | batch_delivery_test.go:246 |
| R11D | r11d-sync-collapse-envelope | sync must forward the full executor envelope | batch_delivery_test.go:140 |
| R11D | r11d-cancel-caller-ctx | control context must NOT be the cancelled caller ctx | batch_cancel_test.go:77 (NoError) |
| R11D | r11d-cancel-false-settlement | held termination keeps the admission unit occupied | batch_cancel_test.go:102 |
| R11D | r11d-sync-cancel-skips-stop | exactly one Stop on caller cancellation | batch_cancel_test.go:177 |
| R11D | r11d-escape-auto-detached-shortcircuit | completion inside the window returns the full envelope | batch_delivery_test.go:275 |
| R11D | r11d-escape-window-defaulted | Expected error with "batch delivery: escape=auto requires an explicit delivery window" in chain but got nil. | batch_delivery_test.go:349 |
| R11D | r11d-drain-block-on-root | Drain ignored the caller's cancellation | batch_delivery_drain_test.go:96 (t.Fatal) |
| R11D | r11d-inline-no-retry-on-failed-ack | An error is expected but got nil. | batch_delivery_test.go:202 |
| R11D | r11d-detached-no-started-election | expected: 1 | batch_delivery_test.go:259 |
| R11D | r11d-store-inline-lost-loses-error | the second inline election must lose | batch_delivery_store_test.go:63 |
| R11D | r11d-store-envelope-ignores-missing | unsettled root has no envelope | batch_delivery_store_test.go:38 |

## Round2 validation report (compressed; superseded by Round5)

Round2/3/4 histories: manifests revalidated with zero ID/file drift; all
targets refreshed to committed SHAs; assertion strings rewritten in Round4 and
re-derived in Round5 (see above). R10c/R11B moved from sibling worktrees into
this integration worktree; R11C/R11D still validate only against their sibling
worktrees and stay excluded from the workflow matrix until their merges land.

## Formerly vacuous tests (resolved this round)

These mutations were previously removed because the named test did not
exercise the mutation's difference. The tests have been strengthened and the
records are restored with real assertions (see Conversion inventory): R10c
m11, R15a mut5 and R7c rc10 (retargeted to
`TestBatchAgentHeldQuestionAfterTurnEnded`). R10c m18 remains excluded (Known
gaps).
