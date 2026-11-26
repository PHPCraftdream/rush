# Rush Verify Design

**Status:** Design proposal; implementation is future work.

**Scope:** Move lane acceptance knowledge from local scripts into a shared `rush verify` command. This document describes behavior and package boundaries only; it does not authorize code changes, staging, commits, or CI dispatch.

**Operator rule:** Shared machinery belongs inside rush rather than in lane-local shell scripts. The existing `rush lock` service is the serialization primitive; verify should build on it rather than invent another machine-wide lock.

## 1. Observed acceptance sequence

The sequence below records the current local acceptance behavior and maps each concern to a proposed verify stage. “Cheap/local” means repository inspection or low-cost validation; “heavy” means toolchain work, external services, or long-running tests. The order is intentionally retained where later checks depend on earlier preparation.

1. **Stage and select the target**
   - The local accept script changes directory to the requested worktree and runs `git add -A`, staging all tracked and untracked changes.
   - This is not a validation check; it changes the index and can stage unrelated work. Classification: mutating/local.
   - Proposed destination: no implicit staging stage. `rush verify` reads a target and computes a changed-file set without changing the index. Whether it accepts dirty and/or committed changes is an open policy question in §7.

2. **Body oracle — non-test code**
   - The body-move oracle compares changed Go files under a supplied package-path prefix, using `HEAD` and the working tree.
   - It normalizes nonblank lines by stripping surrounding whitespace, lowercasing, removing the `agentcall.` and `agent.` qualifiers and collapsing internal whitespace. It compares multisets, so repeated lines retain multiplicity.
   - It reports only-before and only-after residual lines, with a source file and count. The non-test invocation sets the output limit to zero, which truncates each residual list after its first entry and adds an ellipsis marker; the non-test pass therefore effectively shows one residual, not unlimited output. The count summary lines remain complete.
   - Classification: cheap/local compute; informational by default, unless policy makes a residual a failure.
   - Proposed destination: `body-oracle` stage.

3. **Body oracle — tests**
   - A separate invocation selects `_test.go` files and limits each residual report to 40 entries.
   - The separation is significant: production code and test movement are independently inspectable, rather than their lines canceling one another.
   - Classification: cheap/local compute.
   - Proposed destination: same `body-oracle` stage, with two explicit subresults (`non_test` and `tests`) in stage provenance/summary.

4. **Ratchet synchronization**
   - The local script invokes the ratchet synchronizer in dry mode unless `--sync` is supplied. With sync enabled, it stages all changes under `.githooks` after the synchronizer edits them.
   - The synchronizer recomputes indexed-tree counts using the guards' rules. Its three lists cover directory entries (threshold 7), directory files (threshold 21), and files over 900 lines.
   - Existing entries at or below the applicable threshold, or no longer present, are removed. Lower counts are tightened. Growth is reported but not added or raised. Unlisted violations are reported and not added. Selftest count pins are refreshed to match list sizes.
   - Classification: local compute in dry mode; mutating in apply mode.
   - Proposed destination: `ratchet-sync`; dry is the default and `--apply` is required to edit the allowlists and pins. The stage does not stage files.

5. **Guards and selftests**
   - The aggregate guard runner invokes `check_dir_entries.sh`, `check_dir_files.sh`, `check_file_size.sh`, `check_file_size_900.sh`, `check_pkg_names.sh`, `check_layers.sh`, and `check_db_release_pairing.sh`. `check_pkg_names.sh` checks that every non-test, non-`main` Go package directory declares a repository-wide unique package name, with a ratchet allowlist; `testdata` and `package main` are exempt. `check_db_release_pairing.sh` checks that every tracked `*_test.go` calling `db.Connect` or `db.ConnectRead` has at least as many `db.Release`-family calls as connects in that file (it fails only when there are fewer releases than connects); it is a text-count heuristic, not dataflow analysis.
   - It also runs their applicable selftests: directory entries/files, 900-line size, layers, package names, and the 1000-line size selftest. Each result is printed with its return code; any nonzero result fails the aggregate.
   - The layer guard uses the ordered internal groups `kit < store < session < config < agent < app < serve`. Production and test imports are checked, and stale/cleaned exceptions are errors rather than silent exemptions.
   - Guard conventions distinguish pass (0), violations (1), and broken guard inputs (3); broken inputs must never be translated into pass.
   - `check_layers.sh` acquires the legacy shell lock when its local wrapper is present and invokes a bare `go` command otherwise, and `check_dir_entries.sh` collapses malformed input and ordinary violations onto exit code 1. Shelling out therefore cannot satisfy the design's no-bare-go rule or exit-3 broken-input contract; a native port controls both.
   - Proposed destination: `guards` stage, retaining each guard and selftest result, including broken-input distinctions. The guard stage runs guard logic natively in package `verifystatic` as pure-Go ports of the cheap guards (directory entries, directory files, file size, package names, and DB pairing); layers use the Go toolchain through the hardened spawn path. Shell guard scripts remain the pre-push mirror, compared for equal verdicts on seeded violation fixtures (§6).

6. **Test compilation, one package at a time**
   - The local accept flow first obtains packages below the requested Go path prefix with `go list`.
   - It then runs `go test -c -o NUL -gcflags=-e -ldflags='-s -w' <package>` once per package, serially, to reveal compile errors without running tests.
   - Each command uses the memory cap, hardened Go wrapper, and `go-build` lock slot. Failures preserve package identity and a short log excerpt.
   - Classification: heavy.
   - Proposed destination: `test-compile`, with package-level provenance and results.

7. **Build**
   - The local accept flow runs `go build -ldflags='-s -w' .` from the worktree root and records its return code and final diagnostic lines.
   - It uses the same guarded Go execution path and build lock slot as test compilation.
   - Classification: heavy.
   - Proposed destination: `build`.

8. **Vet across three target operating systems**
   - `go vet` is run for `GOOS=linux`, `GOOS=darwin`, and `GOOS=windows`, over the supplied package prefix.
   - The runs are serial, with the Go memory cap and `go-build` lock slot; each operating system gets an independent outcome.
   - Classification: heavy.
   - Proposed destination: `vet`; three OS-specific subresults are required, and a missing target result is not a pass.

9. **Lint across three target operating systems, in chunks**
   - The lint script receives a narrow package-pattern list from its caller and invokes golangci-lint serially for linux, darwin, and windows.
   - Full-tree lint is avoided because it can exhaust the heap. The selected linter is the module-pinned binary in the local tool directory; the PATH binary is older than the module's Go version.
   - Each run is wrapped with the same memory policy and lock, and reports its own result.
   - Classification: heavy.
   - Proposed destination: `lint`, selecting package chunks instead of running full-tree lint.

10. **Format check over changed Go files**
    - The current acceptance script runs `goimports -l` and `gofumpt -l` on the selected Go files, failing on command errors or any listed file.
    - Its selection is flawed after a lane has committed: it unions staged changes against `HEAD` and committed changes against `origin/main`. Since a lane commits before acceptance, the staged-only side can be empty or select the wrong set.
    - Classification: cheap/local, with external formatter binaries.
    - Proposed destination: `format`, using the merge-base branch diff plus working-tree changes, never the index alone.

11. **Whitespace check**
    - The current acceptance flow runs `git diff --check HEAD` and fails on a nonzero result.
    - Classification: cheap/local.
    - Proposed destination: `whitespace`.

12. **Isolated testbed**
    - The testbed script checks a commit out, detached, in a dedicated fixed test worktree, then runs `go vet ./...` and each requested spec serially.
    - Each spec has the form `packages::-run regex` or just a package expression. Tests use `-p 1 -parallel 1 -count=1 -timeout 40m` and the common lock/cap wrappers.
    - It prints a return code per spec and selected first failure/panic lines. Checkout mutates the dedicated worktree; the caller's worktree is not the checkout target.
    - Classification: heavy and stateful with respect to the dedicated worktree.
    - Proposed destination: `testbed`, with an explicit target commit and spec list; it must reject concurrent use of the same testbed checkout. The stage requires both `--commit <sha>` and one or more repeatable `--spec <packages>::<-run regex>` values; it rejects specs without a commit and a commit without specs.

13. **Manual GitHub CI procedure**
    - The operator enables the fork's `ci` workflow, dispatches `ci.yml` with `full=true`, disables the workflow again, and verifies the workflow state becomes `disabled_manually`.
    - The operator watches the run, obtains failed-job logs through the API, re-runs only failed jobs, then posts the result into the PR.
    - This procedure is not implemented by the local scripts. It is an external side-effecting workflow.
    - Classification: external and non-idempotent; dispatch creates runs and toggles workflow state.
    - Proposed destination: `rush verify ci` commands, with each mutation explicit and result recording separate from dispatch. `status --run-id <id> [--watch]` polls until completion when `--watch` is set, emitting one envelope per poll to stdout; with `--json`, it emits one final envelope containing history (one JSON object per line).

## 2. CLI contract

### Local verification

```text
rush verify <worktree-or-cwd> [--stage <name>]... [--packages <pattern>]... [--json] [--changed-only]
```

- `<worktree-or-cwd>` identifies the repository to inspect. `.` or an omitted target may be accepted as cwd only if the command's CLI parser defines that behavior consistently; the documented canonical form names a target.
- `--stage` is repeatable. With no occurrence, the default suite is the ordered local acceptance stages: `body-oracle`, `ratchet-sync` (dry), `guards`, `test-compile`, `build`, `vet`, `lint`, `format`, `whitespace`. - `testbed` is opt-in because it requires a commit and specs; CI is always explicit under `rush verify ci`.
- `testbed` requires both `--commit <sha>` and repeatable `--spec <packages>::<-run regex>` values; reject specs without a commit and a commit without specs.
- Unknown stage names are usage errors, not silently skipped stages.
- Repeated `--stage` values are de-duplicated while preserving first occurrence. The implementation should document this and test it.
- `--packages` is repeatable and accepts Go package patterns. Its meaning is stage-aware: it narrows Go package stages and lint chunks, while stages that work on repository-wide inputs state in the envelope that the option did not apply. Invalid patterns fail the affected stage or target validation; they must not yield an empty successful check.
- `rush verify testbed` requires `--commit <sha>` and repeatable `--spec <packages>::<-run regex>` together with the stage; reject specs without a commit and vice versa.
- The base is the merge-base of the branch and configured upstream base (default `origin/main`, subject to explicit configuration); missing refs or an uncomputable merge-base are target/input errors, never a clean empty diff.
- Without `--changed-only`, each stage uses its documented full scope: for example `vet` uses the requested prefix or patterns, and `guards` evaluates repository-wide guard rules.
- `--json` emits one machine-readable result envelope to stdout. Human diagnostics and progress go to stderr. JSON output must remain parseable on failures.
- Stage selection and order are deterministic. Explicit selection should still run stages in canonical dependency order, not the order in which flags happen to be supplied, unless a future `--preserve-order` option is deliberately designed.

### CI family

```text
rush verify ci dispatch [--pr <number>] [--full]
rush verify ci status --run-id <id> [--watch]
rush verify ci failed-logs --run-id <id>
rush verify ci rerun-failed --run-id <id>
rush verify ci record --run-id <id> [--pr <number>]
```

- All CI commands require visible provenance: exact `gh` argv, target repository/PR/run identifiers, and returned status. Credentials are never included in argv or result summaries.
- `status` reads a known run's state. `--run-id` is required; an absent or malformed identifier is a usage/target error. `--watch` polls until the run completes and emits one envelope per poll to stdout, or with `--json` one final envelope with history (one JSON object per line).
- `failed-logs` reads logs for failed jobs of a run, not every job. The output should summarize each job and include bounded excerpts rather than unbounded logs.
- `rerun-failed` requests reruns only for failed jobs. It is an external mutation and is not implied by `status` or `failed-logs`.
- `record` posts or records the result against a PR. It must identify the PR explicitly or resolve it from the run; the selected behavior is an open question in §7.
- The CI family invokes the `gh` CLI via `platform.Command`; it adds no GitHub SDK dependency. This is a design decision, not an observation from the scripts.
- All CI subcommands accept `--json` and produce the same stage-shaped envelope, with one record per subcommand invocation. `dispatch` returns the exact run ID: parse the numeric ID from the dispatch response, and if omitted confirm it via a run-list lookup keyed by the dispatched event's unique request ID. Echo it as `run_id` in the envelope; require `--json` or have human output print `run id: N`.
- `dispatch`, `rerun-failed`, and `record` require explicit subcommands and confirmation policy. They must never run as side effects of local `rush verify`.

### Result envelope

The JSON envelope has this shape:

```json
{
  "target": "repository identity or user-supplied target",
  "commit": "resolved target commit",
  "base": "resolved comparison base, or empty when not applicable",
  "stages": [
    {
      "name": "guards",
      "status": "pass",
      "duration_ms": 0,
      "exit_code": 0,
      "summary": "all guards and selftests passed",
      "first_failure_excerpt": "",
      "provenance": [
        {"argv": ["bash", ".githooks/check_layers.sh"], "cwd": "."}
      ]
    }
  ],
  "ok": true,
  "started_at": "RFC3339 timestamp",
  "finished_at": "RFC3339 timestamp"
}
```

- The required top-level fields are `target`, `commit`, `base`, `stages`, `ok`, `started_at`, and `finished_at`.
- Every stage has `name`, `status` (`pass`, `fail`, `skip`, or `error`), `duration_ms`, `exit_code`, `summary`, and `first_failure_excerpt`. The excerpt is bounded, for example to 40 lines, and further byte-limited so one pathological line cannot inflate the envelope.
- `provenance` is required even though it is an additional field beyond the specified minimum. It records every exact argv and working directory used, plus relevant environment overrides that affect behavior, with secrets redacted. A stage that legitimately performs only in-process computation records that explicitly as provenance rather than leaving the field absent.
- A stage that was not requested is absent. A selected stage whose prerequisites were omitted or unavailable is `skip` with a reason, not `pass`. A command-start, tool-resolution, or malformed-input problem is `error`; check failure is `fail`.
- `ok` is true only when every requested stage passes. Skips never make a requested acceptance suite green unless the caller explicitly chose a subset.
- `commit` identifies the resolved target commit. Dirty working-tree content is represented by the target/change-set metadata in stage provenance or a future envelope extension; the commit field must not pretend that dirty content is committed.
- Exit codes: `0` all selected stages pass; `1` any stage fails; `2` usage or target error; `3` broken inputs, mirroring the guards' fail-closed convention. If multiple categories occur, usage/target errors take precedence, then broken inputs, then ordinary stage failures.

### Idempotence and mutation policy

- Idempotent and safe to rerun: body-oracle, guards and guard selftests, test-compile, build, vet, lint, format, and whitespace. They may consume time/cache but do not intentionally mutate repository files.
- `ratchet-sync` in default dry mode is read/compute-only and safe to rerun. `ratchet-sync --apply` mutates `.githooks` allowlists and selftest pins and requires explicit consent; it must not stage, commit, or push.
- `testbed` checks out the requested commit in a dedicated worktree and therefore mutates that worktree's state. Re-running is safe only after acquiring a testbed-specific lock and recording/replacing the previous checkout deliberately.
- CI `dispatch` creates runs and toggles workflow state; it is not idempotent. `rerun-failed` creates rerun work; `record` may post or update a PR comment/status and is potentially non-idempotent. `status` and `failed-logs` are read operations.
- No local stage may create a commit, stage files, push, enable/disable a workflow, or silently apply ratchets.

## 3. Code location and package boundaries

### Layer placement

The engine depends on git, the Go toolchain, the gh binary, the repository guard rules, and machinelock/platform services (all at or below serve). It is an ops utility wired from `internal/serve/cmd` exactly like deploy/update, so serve is both the correct import ceiling and the matching neighbourhood.

- Place the implementation in `internal/serve/ops/verify`, alongside the existing `internal/serve/ops/deploy` and `internal/serve/ops/update` tools. The permitted layer groups are `kit < store < session < config < agent < app < serve`; a package under `internal/<group>/` must not import a strictly higher group.
- `internal/serve/ops/verify` depends only on same/lower layers and shared `internal/kit/sys/platform` and `internal/kit/sys/machinelock` services. It must not import beyond serve.
- Command wiring stays in `internal/serve/cmd`, following existing CLI wiring. That package adapts parsed flags to verify request types and renders results; it does not own stage policy or spawn commands directly.
- Reuse `platform.Command` for every external process and `machinelock` for heavyweight serialization. Follow patterns in `internal/agent/safety/lockwrap`; do not introduce a parallel shell-level lock implementation.

### Proposed package and file map

```text
internal/serve/ops/verify/          package verify      (3 files + 4 child directories = 7 elements)
  verify.go  runner.go  target.go
  (verify.go: request, result envelope, provenance, error classes; runner.go: stage registry and runner; target.go: target resolution and changed-file set)
internal/serve/ops/verify/static/   package verifystatic (7 files)
  guards.go  direntries.go  dirfiles.go  filesize.go  pkgnames.go  layers.go  dbpair.go
  (native pure-Go ports of the cheap repository guards)
internal/serve/ops/verify/build/    package verifybuild  (6 files)
  compile.go  build.go  vet.go  lint.go  format.go  whitespace.go
internal/serve/ops/verify/run/      package verifyrun    (3 files)
  body.go  ratchet.go  testbed.go
internal/serve/ops/verify/ci/       package verifyci     (3 files)
  gh.go  dispatch.go  report.go
```

| directory | elements before | elements after |
|---|---:|---:|
| `internal/serve` | 4 | 4 |
| `internal/serve/ops` | 2 | 3 |
| `internal/serve/ops/verify` (3 files + 4 child directories) | 0 | 7 |
| `internal/serve/ops/verify/static` | 0 | 7 |
| `internal/serve/ops/verify/build` | 0 | 6 |
| `internal/serve/ops/verify/run` | 0 | 3 |
| `internal/serve/ops/verify/ci` | 0 | 3 |

The counts follow `.githooks/check_dir_entries.sh`: a directory's elements are direct child code directories plus direct non-generated, non-test source files (so the engine directory counts its four child packages together with its three files); `testdata`, `node_modules`, and `dist` are excluded, generated files are skipped, and repository root is excluded. The counts were derived by reading the guard's counting rules and listing the tracked tree, not by executing the guard; `.githooks/dir_entries_allowlist.txt` was consulted for the serve subtree and has no entries there.

- `verify.go` defines target, selected stages, package patterns, changed-only behavior, explicit mutation options, the envelope types/status, provenance recording, and the error classes that distinguish usage, target, broken-input, and ordinary check failures. JSON field names are stable API and should have golden/contract tests.
- `runner.go` registers canonical stage names, dependencies, default inclusion, and stage-level scope, executes requested stages in dependency order, captures duration and outcomes, and maps them to process exit categories while recording provenance.
- `target.go` resolves cwd/worktree, repository root, commit and comparison base, and computes the union of merge-base-to-branch changes and working-tree changes, preserving path boundaries and rename/deletion semantics.
- `static` contains the native repository guard logic; `build` owns hardened Go/tool invocations and package-scoped results; `run` implements body oracle, ratchet synchronization, and isolated testbed; `ci` is the GitHub CLI adapter and implements dispatch, status, failed logs, rerun, and record operations.
- The map is a proposal, not permission to add the files in this design-only task. Every listed directory is at or below 7 elements (child directories counted) and every proposed file is well under 900 lines.
- Tests count as files for the directory-files guard even when they do not count as directory entries. Budget tests when creating a package. Generated files are not an escape hatch for handwritten behavior; generated markers and counting rules must match repository policy.

### Import, size, and guard constraints

- All imports flow downward or within the serve ceiling. `internal/serve/ops/verify` must not depend on `internal/serve/cmd`; command wiring imports the engine and adapts CLI arguments. No reverse import is allowed.
- Package names `verify`, `verifystatic`, `verifybuild`, `verifyrun`, and `verifyci` were checked unique by reading the package-name guard rule and allowlist (three records in 12 lines including comments, none for any `verify*` name) and searching the tracked tree for existing declarations; no exemption is needed.
- The layout was checked against `.githooks/check_layers.sh`, `.githooks/check_dir_entries.sh` (with `.githooks/dir_entries_allowlist.txt` consulted for the serve subtree, which has no entries there), and `.githooks/check_pkg_names.sh` by reading them, not by running them. Running `check_layers.sh` would invoke Go, which this design round must not do.
- The layers implementation uses the same `go list -json` input and ratchet semantics through `platform.Command` plus `machinelock`.

## 4. Heavy-stage serialization and memory caps

### Execution invariants

- Every external process spawn goes through `internal/kit/sys/platform.Command`. No `exec.Command`, `os/exec.Command`, shell command, or direct process API may bypass the shared platform wrapper. The repository's `TestNoUnhardenedProcessSpawns` is a required regression guard for every stage addition.
- Every Go tool invocation is wrapped with the existing lock wrapper pattern and acquires one `machinelock` slot. Initial slot names follow `go-build` and `go-analysis`; ordinary build, test compile, vet, and lint use `go-build`. Audit-like `go run` work may use `go-analysis` if it is added later.
- Each Go child receives `GOTOOLCHAIN=auto`, `GOMEMLIMIT=700MiB`, `GOMAXPROCS=2`, and `GOFLAGS=-p=1`, subject to deliberate per-gate overrides that are also recorded in provenance.
- Package compilation is one package per `go test -c` command. Test execution uses `-p 1 -parallel 1`; do not trade away serial execution to reduce wall time.
- The lock is acquired before starting the capped process and held until the process exits. Cancellation and timeout paths must release it through the lock service's normal lifecycle.
- Stage concurrency at the orchestrator level must not permit two heavy Go commands to bypass the shared slot. A stage may fan out only into work queued by the same policy; the default is sequential.
- Resolve tools before execution and fail closed on missing required tools. Do not report `skip` as success when a required tool is missing.

### Four explicitly avoided failure modes

1. **Full-tree golangci-lint heap crash.** Lint uses caller-selected package chunks, runs chunks serially for each GOOS, and reports the exact package patterns. No default full-tree lint invocation is permitted.
2. **PATH golangci-lint older than the module's Go version.** Resolve the module-pinned version, for example through `go run` of the pinned module version or a tool-dependency build. Never trust an unqualified PATH binary. Record the resolved tool version and source in provenance.
3. **Windows `xargs` path corruption.** Do not use `xargs` for path lists. Build argv in Go and pass each path as a separate argument via `platform.Command`; preserve spaces and backslashes without shell reinterpretation.
4. **Acceptance format check sees only staged files after commit.** The Go implementation derives changed files from the merge-base diff of the branch and unions current working-tree changes. The index is not the source of truth. Add a regression case with a committed changed Go file and an empty index.
5. **Bare Go commands under memory pressure.** No Go command is spawned bare. Every spawn uses `platform.Command`, the correct `machinelock` slot, `GOMEMLIMIT`, and `-p 1` (including Go commands hidden inside helper paths). A source-level test must fail if an unguarded spawn is introduced.

### Resource budgets

The initial budgets mirror `.githooks/pre-push-budgets.sh`; they are per-gate caps, not a promise that every platform enforces them identically.

| Gate | Memory cap | Timeout | Intended use |
|---|---:|---:|---|
| Build | 3g | 600s | Build and related compile work |
| Lint | 3g | 900s | One bounded package chunk and GOOS at a time |
| Agent | 2g | 600s | Agent-specific targeted checks, if used |
| Remainder | 3g | 1800s | Other longer gates, including selected test work |
| Process | 2g | 300s | Spawn-guard and process semantics tests |

- The testbed's per-spec Go test timeout remains 40 minutes at the Go test level; the outer process budget must be designed to allow that explicit test timeout rather than silently truncating it.
- Existing local wrappers also use a 2g cap for acceptance Go commands and the golock wrapper has 4g for the build slot / 3g for analysis. Verify must reconcile those values with the per-gate budgets rather than layering contradictory caps. The table above is the pre-push budget baseline; rollout must preserve tested platform behavior.
- Caps are enforced via platform-specific process controls where available. The design does not assume that an environment variable alone is a hard resident-memory limit.
- Report timeout, lock wait, memory policy, and actual command exit separately. A lock timeout is not a successful skipped stage.

## 5. Smallest vertical slice

### Slice 1: guards end to end

1. Add only the `guards` stage path through CLI parsing, target resolution, stage execution, JSON envelope serialization, and exit-code mapping.
2. Run guard logic natively in package `verifystatic`: pure-Go ports of the cheap guards (directory entries, directory files, file size, package names, and DB pairing); layers use the Go toolchain through the hardened spawn path. Keep shell scripts as the pre-push mirror and compare equal verdicts on seeded violation fixtures (§6).
3. `check_layers.sh` acquires the legacy shell lock when its local wrapper is present and invokes a bare `go` command otherwise, while `check_dir_entries.sh` collapses malformed input and ordinary violations onto exit code 1. Shelling out cannot satisfy the no-bare-go rule or exit-3 broken-input contract; a native port controls both.
4. Preserve fail-closed inputs. Return exit code 3 for broken guard inputs, not generic pass and not an unqualified skip.
5. Test a clean fixture and seeded violation trees. Confirm violations yield `fail`, the stage has meaningful provenance, and the overall exit is nonzero; test malformed/missing inputs as `error`/broken-input exit 3, never `pass`.
6. Verify every child process uses `platform.Command`; add the new spawn sites to the source-level spawn guard's expected inventory only through its normal reviewed mechanism.
7. Wire `rush verify <target> --stage guards --json` in `internal/serve/cmd` and demonstrate parseable JSON on both pass and fail.

### Migration order after the vertical slice

1. **Vet** — retire the vet portion of the local accept script when three-GOOS results, correct package scope, locks, caps, and provenance are in rush verify.
2. **Build** — retire the local build portion when the same flags and hardened invocation are covered by tests.
3. **Test-compile** — retire the per-package compile loop when one-package-at-a-time behavior and package-level failures are proven.
4. **Format** — retire the local goimports/gofumpt portion only after the merge-base-plus-working-tree changed set fixes the staged-after-commit flaw.
5. **Lint (chunked)** — retire the local lint script only after package chunking, pinned tool resolution, three-GOOS serial execution, and memory behavior are verified.
6. **Testbed** — retire the local testbed script only after dedicated worktree locking, commit checkout, per-spec serialization, and result extraction are tested.
7. **Body oracle** — retire the Python oracle only after Go matches the normalization, multiset, rename/map, test split, limit, and deleted/new-file semantics.
8. **Ratchet-sync (dry first)** — retire the Python synchronizer only after dry output matches existing rules. Keep apply gated behind explicit `--apply` until operator policy is approved.
9. **CI family** — replace the manual procedure only after `gh` operations, workflow toggle recovery, failed-job targeting, rerun, result recording, and fake-CLI tests work.

### Script retirement rule

- A local script is not retired merely because a command exists. The corresponding verify stage must have sentinel coverage, provenance assertions, failure-path tests, and have been used successfully by **N consecutive lanes**.
- Recommend `N = 3` for local acceptance replacements, with the CI family also requiring at least one observed recovery path on failed jobs. This is a concrete proposal for operator approval, not an existing policy.
- If a stage is changed during the observation window, restart its count. Record each lane's commit and stage result in normal lane records; do not add a machine-specific ledger to this document.
- Until retirement criteria are met, the script remains the reference behavior and verify is run in comparison mode where feasible. A discrepancy blocks retirement and is reported, not automatically “fixed” by changing the baseline.

## 6. Tests that prevent a vacuous pass

A stage that returns `ok` without actually checking anything is the principal design risk. Tests must prove execution and failure sensitivity, not merely exercise result formatting.

1. **Exact command provenance**
   - Every external command actually invoked is echoed into the stage envelope as exact argv, cwd, and relevant environment overrides.
   - Tests assert argv is non-empty and matches the intended command and arguments. For command families, assert the expected executable and required flags, not an overly brittle full string when platform quoting differs.
   - Arguments containing spaces, Unicode, and Windows-style separators are tested as individual argv entries; no path is reconstructed by splitting a shell string.

2. **Required provenance field**
   - `provenance` is mandatory on every stage result, including `skip` and `error` outcomes.
   - In-process stages record an explicit operation descriptor, such as changed-file enumeration or line-multiset comparison, and counts of examined inputs.
   - JSON decoding tests reject an omitted or null provenance field for registered stages.

3. **Registry-wide provenance test**
   - A top-level test iterates every registered stage using a controlled fake environment and asserts the resulting stage record has non-empty provenance.
   - A newly registered stage therefore cannot silently inherit an empty success path.
   - Explicit skip reasons are also asserted, so unconfigured or unsupported execution cannot be disguised as pass.

4. **Broken fixture sentinel**
   - Point each check stage at a deliberately broken fixture, such as a source file violating a guard limit or malformed guard input, and assert it returns failure/broken-input status.
   - A pass on a broken input proves the stage is dead or is inspecting the wrong target; the test must fail loudly.
   - The fixture must be minimal and generated under a test temp directory, not written into the live repository.

5. **Seeded guard violation trees**
   - Build fixture trees that exceed the seven-entry limit, exceed the 21-file limit, exceed size thresholds, contain a duplicate package name, contain an upward layer import, and violate database release pairing.
   - Confirm the matching guard fails and unrelated guards are still represented in the aggregate result.
   - Include stale and malformed ratchet entries to verify cleanup and broken-input semantics.

6. **Changed-set regressions**
   - Commit a changed Go file on a fixture branch, leave the index empty, and prove `format` and body-oracle selection includes it.
   - Add an unstaged modification and prove it joins the committed branch diff.
   - Cover added, modified, deleted, renamed, and path-with-spaces cases. Ensure deletions are not silently dropped from whitespace/body comparisons.
   - Assert an empty result is accepted only when Git successfully computed an actually empty union.

7. **Stage-specific sentinels**
   - `body-oracle`: fixture with a changed normalized line must produce a nonzero residual; equivalent qualifier/whitespace variants must normalize as designed.
   - `ratchet-sync`: a fixture list with a reducible count must propose removal/lowering in dry mode; dry mode must leave bytes untouched; apply mode must alter only the three target lists and corresponding pins.
   - `test-compile`: deliberately uncompilable package fails with package provenance; no packages selected must be an explicit error or skip according to request validation, never pass.
   - `build`, `vet`, and `lint`: fake runner captures required args; a failing runner returns `fail`; missing pinned linter returns error rather than PATH fallback.
   - `format`: a deliberately unformatted file is detected; a missing formatter does not become pass.
   - `whitespace`: trailing whitespace and conflict-marker cases are detected on the target change set.
   - `testbed`: fake Git/worktree runner verifies commit checkout, lock acquisition, and serial spec argv; a failed spec remains visible even if a later one passes.

8. **CI with a fake `gh`**
   - Place a fake `gh` executable on the test PATH and drive dispatch, status, failed logs, rerun-failed, and record.
   - Assert the command sequence, arguments, parsed IDs, workflow disable confirmation, and error recovery when dispatch, disable, or status fails.
   - Verify no command leaks credentials and no unrelated jobs are rerun.
   - Test that status and failed-logs do not issue mutating requests; rerun/record/dispatch do not run without their explicit subcommand.

9. **Exit-code contract tests**
   - All pass => 0; a check failure => 1; invalid flags/target => 2; broken guard inputs => 3.
   - Multiple failure classes follow precedence in §2. Tests assert both process status and JSON stage state.
   - Truncated excerpts preserve the first failure lines, indicate truncation, and remain valid JSON even for invalid UTF-8 command output.

10. **Spawn and serialization tests**
    - `TestNoUnhardenedProcessSpawns` covers every verify spawn. Verify-specific tests inject a fake `platform.Command` runner rather than spawning arbitrary commands.
    - A fake lock asserts heavy commands acquire the correct slot and never overlap. Cancellation and timeout release the lock.
    - Tests assert required Go environment and `-p 1`; Go test invocations additionally require `-parallel 1`.
    - A test that omits the lock, memory limit, or platform wrapper must fail by construction, not merely because a particular machine happens to have spare RAM.

## 7. Reliability

### Evidence and unproven claims

The following statements remain **UNPROVEN** by this design exercise because no scripts or commands were executed; they are observations from source review and the supplied verified summary, not runtime validation:

- **UNPROVEN — CI workflow toggle semantics:** dispatching the fork workflow, disabling it afterward, and observing the `disabled_manually` state behave as described. The operator's manual procedure is not implemented in the scripts read here.
- **UNPROVEN — CI API shape and permissions:** the exact `gh` commands, required authentication scopes, job-log retrieval limits, selective rerun semantics, and PR result-posting behavior. No CI command script was read because the procedure is manual.
- **UNPROVEN — pinned golangci-lint mismatch:** the local lint script states that PATH's binary is older than the module's Go version and uses a module-pinned binary. This exact mismatch was not independently checked by running either binary.
- **UNPROVEN — `run_capped.ps1` enforcement:** the pre-push budget file assigns caps and the fast hook invokes `run_capped.ps1` on Windows; hard memory and timeout enforcement, process-tree handling, and exit-code behavior have not been executed or validated here.
- **UNPROVEN — rush lock runtime semantics:** source and the supplied summary describe machine-wide FIFO serialization and lock slot behavior; actual cross-process contention, fairness, stale-owner recovery, and timeout behavior were not exercised.
- **UNPROVEN — platform.Command coverage:** the design requires all verify spawns to use it and references `TestNoUnhardenedProcessSpawns`; this session did not execute that test or inspect every existing spawn site.
- **UNPROVEN — guard behavior on current trees:** guard limits, layer direction, ratchet rules, selftest coverage, and fail-closed codes are stated by the scripts, but no guard or selftest was run.
- **UNPROVEN — cross-platform Go behavior:** linux/darwin/windows vet and lint invocations are visible in the scripts, but none were executed and no claim is made that the present worktree passes them.
- **UNPROVEN — testbed isolation:** the script checks out a commit into a dedicated fixed worktree and serializes specs, but collision safety, recovery after interruption, and repeat-run behavior have not been tested.
- **UNPROVEN — body-oracle correctness:** normalization and multiset behavior are visible in the source, but no fixture comparison was run and the design has not established parity for every rename/deletion edge case.
- The accept script tracks one `failed` flag for test-compile/build/vet/format/whitespace but does not fold in body-oracle, ratchet-sync, or aggregate-guard outcomes, so a guard failure does not by itself fail the acceptance run. The testbed script prints per-spec return codes and failure excerpts but never aggregates vet or test failures into its process exit status, so a lane can read green from prose while the process exits zero.
- **UNPROVEN — format changed-set flaw impact:** the acceptance script's selection combines staged-versus-HEAD and committed-versus-main diffs; the documented flaw follows from that selection logic and lane timing, but no failing acceptance example was run.

### Scripts read

Read for this design: the local accept script, testbed script, lint script, ratchet synchronizer, body-move oracle, aggregate guard runner, Go lock wrapper, `.githooks/check_layers.sh`, `.githooks/check_dir_entries.sh`, `.githooks/pre-push-budgets.sh`, and `.githooks/pre-push-fast.sh`. `check_pkg_names.sh` and `check_db_release_pairing.sh` are described from a reviewer-verified summary and were cited in review against the scripts; they are not in the author's own read inventory.

Not read: no requested script was intentionally omitted. Other guard implementation files were not individually read in this session; their names, coverage, thresholds, and selftest membership are taken from the supplied verified summary and aggregate guard runner. The GitHub CI procedure was supplied by the orchestrator and has no corresponding script in the read set.

### Known gaps at landing

The design landed after two review rounds (the stop rule for scouting documents); these P2 gaps remain and belong to the implementation task's first step:

- Exact CI run identification when `gh workflow run` returns no run id: the checked-in workflow declares only the `full` input and does not put a correlation token in the run name, so the dispatch-correlation fallback needs a workflow change (a `run-name` or input carrying a request id) that this document does not specify.
- The CLI examples are not fully consistent: `testbed` appears both as an opt-in stage and as `rush verify testbed`; `ci status --watch` is specified as JSONL history in one place and as one final envelope in another; the guards envelope example still shows a shell invocation although the first slice is native.

### Open questions for the orchestrator

1. **Should `rush verify` stage/commit changes or accept an already-committed branch?** Recommendation: never stage or commit; accept both committed branch changes and working-tree edits via merge-base diff union. This is non-destructive and fixes the current format selection flaw.
2. **Does the CI family require existing `gh` authentication?** Recommendation: yes; detect auth before any mutation, provide a clear error, and never initiate an interactive login from verify.
3. **Where do lock slots live if `golock.sh` is retired?** Recommendation: use the existing `machinelock` service with stable names `go-build` and `go-analysis`; keep slot-name ownership in the shared verify/tooling contract, not a local script.
4. **What is the ratchet apply policy?** Recommendation: dry by default, require explicit `--apply`, print a complete diff before mutation, and never stage or commit the changed `.githooks` files.
5. **Are memory caps Windows-only or cross-platform?** Recommendation: keep the same logical per-gate budget on every platform, use hard process caps where supported, and report when a platform offers only best-effort memory control rather than claiming enforcement.
