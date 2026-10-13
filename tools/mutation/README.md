# Temporary mutation proof

Push `mut/<name>` from the **exact integration commit** being proved, with the
workflow, runner and manifests present. Only `mut/**` pushes trigger Actions;
no main/PR/dispatch/schedule trigger. Permissions are read-only, concurrency does
not cancel executing runs; replaced pending runs remain unproved. Record the
checkout SHA: results do not prove later commits.

Inspect Actions step summaries and download per-shard results and JSON/stderr
artifacts, including failed jobs. Upload uses the new standard v4.6.2 pin
`actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02`;
it is not independently provenance-verified. There was no tracked repository
upload-action precedent.

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

## Conversion inventory and gaps

- R7c: 15 records, including distinct rc4a/rc4b. Batch2/batch2c supply the
  direct scope primary for rc4a; alternate advertised tests remain notes.
  rc10's first named primary does not end its turn while held; the later
  after-turn-ended test is stronger but is not silently substituted.
- R10c: 14 available of 24 planned. Missing: **m15, m16, m17, m18, m19, m20,
  m21, m22, m23, m24**. SQL-source edits are translated to compiled sqlc Go
  query text (positional parameters, slice markers, omitted semicolon).
  m13 changes only the emitted announce arm, not both arms described in its
  header. m11's named fixture contains no relevant void row, so no distinctive
  failing assertion is determinable. The sibling initially had compiled SQL
  drift; its HEAD advanced externally during conversion. Re-read-only `git show`
  supplied the new compiled SQL hash; no source needle was changed to fit it.
- R11B: 17 emitted scripts converted, **not the generator**. m02's actual
  replacement assigns empty root and skips UUID generation, contrary to its
  header. m17 changes a root UUID without changing the child node ID; an earlier
  Add error can precede the advertised root-count assertion. Neither has saved
  output establishing a distinctive failing assertion.
- R15a: 8 records. mut2/mut7 are actual named Node/TypeScript tests, unsupported
  by the Go-only v1 schema/runner: preserve and report them, never fabricate Go
  killers. mut3's three-edit helper/import transformation is encoded as one
  inline equivalent byte-substring parser at the original OutcomeCode needle.
  mut5's fixture has identical ordinal and NodeID sort orders, so a distinctive
  failing assertion is not determinable. No local Go/Node mutation tests run.
- **R10b is out of scope and missing from this manifest set**: R10b.json and its
  reviewed mutation inventory/proof records are not supplied by this task.
  Do not present these 54 records as the full approximately-75-scenario proof.

## Local checks

From the repository root (replace `<manifest-name>` with a supplied manifest):

```bash
python -I tools/mutation/run.py --selftest
python -I tools/mutation/run.py --validate --root . --manifest "tools/mutation/<manifest-name>.json"
```

`--selftest` uses canned Python fixtures without invoking Go. `--validate` is a
cheap, read-only manifest/source check: no Go commands, checkout copies, mutation
runs, `results.json`, logs, artifacts, or step-summary writes, even on failure.
No scratch working directory is required. Exit zero means validation passed,
not that any mutation was proved. Unsupported R15a Node tests and empty assertion
blockers remain preserved in the manifests and produce validation failures.

## Validation report (independently re-run verification)

| Task | Records | Fully valid records | Invalid records | Diagnostics |
|---|---:|---:|---:|---:|
| R7c | 15 | 14 | 1 | 1 |
| R10c | 14 | 13 | 1 | 1 |
| R11B | 17 | 15 | 2 | 2 |
| R15a | 8 | 5 | 3 | 7 |

Independent re-run verification: selftest PASS with zero failures.
All four manifest validations exited 1, with zero `needle-not-exactly-once`
and zero `sha-mismatch` diagnostics. `mutations ok` counts only fully valid
records; invalid records are counted once by manifest index, separately
from diagnostic count. Verified blockers:
R7c rc10, R10c m11, R11B m02/m17, R15a mut5 have empty expected assertions;
R15a mut2/mut7 each have an invalid Go test name, non-internal package, and
non-Go target. These limitations are explained above and in record purposes.
No needle was changed to force validity. R10c's initial ten integrity failures
became ten SHA mismatches after an external commit, then cleared after its new
HEAD blob was read and hashed; its final HEAD is
`4f49c6716fd9bc398710ea18115f9943a6ae53e6`.
R7c/R15a source HEAD: `81887a430010ab5a18644536ae7df1bf3f51f598`;
R11B source HEAD: `0fc7b17375c2bfa1117f2537403d0f06b7ce11be`.
