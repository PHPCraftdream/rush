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

- R7c: 15 records, including distinct rc4a/rc4b. The original rc10 primary
  remains `TestBatchAgentHeldQuestionKeepsNodeLive`; no alternate is substituted.
- R10b: all 17 supplied records from `.mutsrc/r10b.mutlist.json`, with supplied
  IDs, needles, replacements, tests, purposes and expected assertions unchanged.
  Only `./internal/agent` is normalized to `internal/agent` and the LF HEAD blob
  hash is added. Workflow shards are 1/2 and 2/2. m16 remains an uncertain killer:
  initial evaluation can race with root completion and satisfy ANY before a hint.
  A survivor is a test finding, not a runner defect; do not silently strengthen
  the fixture or reclassify survival as proof.
- R10c: 14 records; SQL-source mutations target equivalent compiled sqlc Go
  query text without regeneration. m13 changes only the emitted announce arm.
- R11B: all 17 emitted scripts, not the generator. m02/m17 now use the earlier
  primary-test NoError failure, not their advertised later root-count messages.
- R15a: 6 Go records after removing mut2/mut7. mut3 retains the inline equivalent
  of its helper/import parser transformation; no target needles were altered.

## Known gaps

R10c **14/24**: missing **m15, m16, m17, m18, m19, m20, m21, m22, m23, m24**.
Those scripts were never delivered. Merge a future `.mutsrc-r10c.json` inventory
when supplied; do not invent the missing records. R10c and R11B source/tests
still reside in sibling worktrees, read-only for this conversion. Validation
there is not proof against the final integration checkout.

### Named-test assertion review

These are source-derived assertion choices, not observed mutation executions.
Empty assertions remain intentional validation blockers for NO-ASSERTION.

| Record | Choice | Exact assertion anchor and reason |
|---|---|---|
| R7c rc10 | NO-ASSERTION | `internal/agent/batch_exec_agent_test.go:285`: `batchAgentTestNoReport(t, reports, "a question is held")`. The original primary does not end the turn while held, so both closure predicates remain false. |
| R10c m11 | NO-ASSERTION | `internal/session/batch_history_snapshot_test.go:41`: `require.Equal(t, 3, int(got.Debt), "settled unreacted woken nodes are debt")`; line 61: `require.Zero(t, reacted.Debt)`. No void unreacted woken row exists in this fixture. |
| R11B m02 | `jobbatch: commit failed: error` | `internal/session/batch_commit_test.go:305` calls `commitHookSubmit`; helper line 119 is `require.NoError(t, err)`. Empty root reaches the identity mismatch at `batch_commit.go:114-115`, before line 306's root count. |
| R11B m17 | `jobbatch: commit failed: error` | `internal/session/batch_commit_test.go:330`: `require.NoError(t, err)` after Add. The new UUID has neither root nor parent required by migration `20261012000001_batch_projection.sql:66-69`; the insert fails before the root-count assertion. |
| R15a mut5 | NO-ASSERTION | `internal/server/handlers_batch_test.go:159`: `require.Equal(t, []int64{0, 1, 2}, got, "nodes must be ordered by ordinal, not arrival or NodeID")`. Fixture lines 149-151 has the same ordinal and NodeID order. |

R11B's error substring is anchored in `internal/jobbatch/errors.go:20`
(`ErrCommitFailed = errors.New("jobbatch: commit failed")`) and
`internal/jobbatch/runner_commit.go:88,110,298`: a hook error retains reason
`error`, and `signalFault` formats `%w: %s`. m02 assigns empty root and skips
UUID generation; it does not generate a replacement UUID. m17 changes root_id,
not the deterministic child node_id. `batchStoreTestDB` uses `db.Connect` with
foreign keys ON. No test name is used as an assertion substring.

## Not provable in this workflow

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
python -I tools/mutation/run.py --validate --root . --manifest tools/mutation/R15a.json
python -I tools/mutation/run.py --validate --root <R10c-sibling> --manifest tools/mutation/R10c.json
python -I tools/mutation/run.py --validate --root <R11B-sibling> --manifest tools/mutation/R11B.json
```

`--selftest` uses canned Python fixtures without invoking Go. `--validate` is
read-only: no Go commands, checkout copies, mutation runs, results, logs,
artifacts or step-summary writes. Exit zero means validation, not mutation proof.

## Round2 validation report

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
ambiguity. Installed PyYAML parsed it successfully via a Python `-I -c` check;
Python also verified all **9** matrix manifest references exist. No dependency
was installed. No local Go/Node tests or mutation proofs were executed; only
Python checks and read-only git queries ran. Runner remains unchanged.

## Suspected vacuous tests (static analysis; not proven)

These mutations were removed from the manifests because the named test, read statically, does not exercise the difference the mutation introduces (no assertion can be named). Each is a coverage finding for the task owner: strengthen the test so the mutation dies, then restore the record with a real `expected_assertion`.

- R10c `m11_snapshot_debt_counts_void` — `TestBatchHistorySnapshotShapeAndInjectableHostVerdict`: NO-ASSERTION after reading the current sibling's named TestBatchHistorySnapshotShapeAndInjectableHostVerdict: batch_history_snapshot_test.go:41 compares debt to 3 with settled unreacted woken nodes are debt; line 61 requires zero after reacted=1. No void unreacted woken row is seeded, so removing the void guard cannot fail either assertion. Preserve this primary and the empty assertion; no substitute test or claimed kill.
- R15a `mut5_ordinal_order` — `TestBatchWorkSnapshot_NodesOrderedByOrdinal`: NO-ASSERTION after reading the named test: handlers_batch_test.go:159 compares ordinals with the message nodes must be ordered by ordinal, not arrival or NodeID, but fixture lines 149-151 orders r1, r1-a, r1-c identically under both comparators. This mutation cannot fail that equality; keep the assertion empty rather than invent a killer.
- R7c `rc10_held_not_live` — `TestBatchAgentHeldQuestionKeepsNodeLive`: NO-ASSERTION after reading the original primary TestBatchAgentHeldQuestionKeepsNodeLive: batch_exec_agent_test.go:285 calls batchAgentTestNoReport for a question is held, but the turn has not ended while held, so both closure predicates remain false. Preserve this primary and the empty assertion; do not substitute the after-turn-ended test.
