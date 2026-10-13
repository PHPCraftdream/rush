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
- R11E: 10 records from `.mutsrc/lists/r11e.mutlist.json`, packages normalized
  to `internal/app`; one workflow shard (single package, 10 mutations).
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

## Not provable in this workflow

- R10c m18: excluded with an empty assertion; its kill is scheduling-dependent
  (see Known gaps for the author's reason).

Removed R15a mut2/mut7 target actual Node/TypeScript tests in
`web/src/batchWork.test.ts`, not Go tests:

- mut2: `covered flat rows are replaced by structured nodes; structured-only nodes appear`;
  line 161 is `assert.equal(tc1.status, "running")`.
- mut7: `incomplete snapshot merges by rootID and never drops live roots`;
  lines 87-90 deep-compare sorted root IDs with `["r1", "r2"]`.

The existing `.github/workflows/lint.yml` `lint-web` job already covers these
web unit tests through `node --test src/*.test.ts` (lines 106-108), alongside
pnpm typecheck/lint. That ordinary coverage does not prove either mutation.
Both remain unproven without a future Node mutation runner; none is added here.

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

## Round4 validation report (current round)

Round4 fix pass: every `expected_assertion` re-derived as a LITERAL substring
of the failing test's output (custom message text, or a short certain
testify-printed literal; verified against testify v1.11.1:
same-type Equal prints `%#v`, so `expected: 1` for int64(1), and differing
types print `%T(%v)`, so `expected: int(1)`).

| Task | Root | Records | OK | Invalid | Needle | SHA | Diagnostics | Exit |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| R10b | base | 17 | 17 | 0 | 0 | 0 | 0 | 0 |
| R10c | base | 20 | 20 | 0 | 0 | 0 | 0 | 0 |
| R11B | base | 17 | 17 | 0 | 0 | 0 | 0 | 0 |
| R15a | base | 6 | 6 | 0 | 0 | 0 | 0 | 0 |
| R7c | base | 15 | 15 | 0 | 0 | 0 | 0 | 0 |
| R11E | base | 8 | 8 | 0 | 0 | 0 | 0 | 0 |
| Teardown | base | 5 | 5 | 0 | 0 | 0 | 0 | 0 |
| R11C | r11c sibling | 11 | 11 | 0 | 0 | 0 | 0 | 0 |
| R11D | r11d sibling | 12 | 12 | 0 | 0 | 0 | 0 | 0 |

Selftest PASS (0 failures); `actionlint .github/workflows/mutation-check.yml`
exit 0.

Changed assertions (summary — all were narrative or non-printing, now the
verbatim failing-output literal; full id-by-id rationale in the PR/report):
R11E 7 rewritten (fault-exit-code-zero, fault-watcher-ignores-runner,
host-release-error-inverted, close-timeout-releases, writer-release-dropped,
fault-during-drain-not-forced, setrunner-wake-dropped); R11C all 11
rewritten; R11D all 12 rewritten. R7c/R10b/R10c/R11B/R15a/Teardown re-checked
against test sources and testify formatting: all existing values already
literal, kept unchanged.

Dropped/retargeted records:
- R11E `fault-fence-dropped`: DROPPED. Fenced() short-circuits on the attached
  Runner's closed Faulted channel, and every named fixture fires the fault
  channel before asserting Fenced, so removing `fenced.Store(true)` is not
  distinguished (no test exercises fence-before-channel-closure, e.g. Stop()
  before fault).
- R11E `release-writer-fence-check-dropped`: DROPPED. In
  TestShutdownRacingFaultReleaseAbortsMidway the fault fires inside
  ReleaseHostIdentity, so the fence re-check before the writer release returns
  before the mutated `if true || ...` line is ever reached: the mutant is
  unreachable in the named test.
- R11E `fenced-channel-check-dropped`: RETARGETED. The old `_ = r` replacement
  left no return in Fenced() (compile error). Replacement is now
  `\t_ = r\n\treturn true` (Fenced always true when a runner is attached),
  killed by TestOrdinaryShutdownDrainsBeforeRelease (runner attached, no
  fault: Forced must be false — assertion `ordinary shutdown is graceful`).

Non-compile scan of every `_ =`/condition replacement across all manifests:
R10b m12 (`outErr = ...`) — `outErr` is returned at batch_await.go:204, OK;
R10b m01/m06 append to named-return slices, OK; R11E fault-watcher-ignores-runner
(`if false {`) keeps the body's runner references, OK; R7c rc9 `select {}` is a
terminating statement, OK; R15a mut3 inline closure returns in all paths, OK;
R10b m08/m09 use `live.Tree` in scope, OK. The only compile error found was the
old R11E fenced replacement, fixed above.

Short stable fallback literals used (custom message not printed by the first
failing assertion): R11C `M8-policy-scope-drop` -> `Not equal:` (bare
require.Equal on a context value; no message argument, expected value not
statically known); R11C `M11-waiter-poison-replay` -> `Received unexpected error`
(bare require.NoError on the poisoned waiter's error); R11C `M6-notwired-fallthrough`
-> `invalid memory address or nil pointer dereference` (nil-seam panic text);
R11D `r11d-detached-no-started-election` -> `expected: 1` (bare Equal on the
election counter).

Excluded records: R10c m18 only (as in Round3). R11E now carries 8 records
(was 10).

## Round2 validation report (superseded by Round4)

| Task | Records | OK | Invalid | Needle | SHA | Diagnostics | Exit |
|---|---:|---:|---:|---:|---:|---:|---:|
| R7c | 15 | 14 | 1 | 0 | 0 | 1 | 1 |
| R10b | 17 | 17 | 0 | 0 | 0 | 0 | 0 |
| R15a | 6 | 5 | 1 | 0 | 0 | 1 | 1 |
| R10c (sibling) | 14 | 13 | 1 | 0 | 0 | 1 | 1 |
| R11B (sibling) | 17 | 17 | 0 | 0 | 0 | 0 | 0 |

Selftest PASS (zero failures). The only diagnostics are empty assertions for
`rc10_held_not_live`, `m11_snapshot_debt_counts_void`, `mut5_ordinal_order`.
Drift: **zero IDs/files**, zero non-unique needles and zero SHA mismatches across
all five validations. No existing target hash was refreshed and no needle was
changed. Roots checked at these read-only HEADs:

- R7c/R10b/R15a: `ef00333deb116c4c8607a37f46ebbf1d6e4a9594`.
- R10c sibling: `4f49c6716fd9bc398710ea18115f9943a6ae53e6`.
- R11B sibling: `0fc7b17375c2bfa1117f2537403d0f06b7ce11be`.

R10b's `internal/agent/batch_await.go` LF HEAD blob SHA-256 is
`2222eb6470b818e3ccf40ebc809c4c2747dd3301dcd0620c85fbf72ed7f834ef`;
read-only `git show` bytes contained no CRLF pairs. Python verified all 17
supplied records' fields match, apart from package normalization and added hash.

Workflow's commit-recording `run` uses a block scalar to avoid colon-space YAML
ambiguity. Python verified all **11** matrix manifest references exist. No
local Go/Node tests or mutation proofs were executed; only
Python checks, actionlint and read-only git queries ran. Runner remains unchanged.

## Formerly vacuous tests (resolved this round)

These mutations were previously removed because the named test did not
exercise the mutation's difference. The tests have been strengthened and the
records are restored with real assertions (see Conversion inventory): R10c
m11, R15a mut5 and R7c rc10 (retargeted to
`TestBatchAgentHeldQuestionAfterTurnEnded`). R10c m18 remains excluded (Known
gaps).
