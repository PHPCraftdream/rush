# ci-structure — ACTIVE tiered CI

`.github/workflows/ci.yml` is the active CI entry point for PRs targeting `main`, pushes to main/retained shadow branches, and manual dispatch. Retired unconditional build/lint/security workflows are historical only. Separate-purpose release, publish-fork-npm, nightly (schedule disabled), schema-update, labeler, diagnostics, and reusable/standalone ktav-purity and licenses workflows remain.

## Job graph

```
plan -> checks; plan + checks -> tests (test-ubuntu, agent-ubuntu, race-ubuntu, test-native, windows-full-shards)
plan -> native-lint (macOS/Windows matrix); plan -> web/security/ktav-purity/licenses
all -> ci-gate (live PR state, plan contract, top-level needs, exact current-attempt leaf jobs)
```

## What each job runs and why

- **plan** validates base/source-head identity, extracts Linux/darwin/windows Go graphs when needed, runs selector and promotion, uploads `plan.json`, and exports validated outputs. Main pushes and main-bound PRs with live `full-ci` promote all OS package owners, configured races, web and full security. An unlabeled main-bound PR uses the minimal plan.
- **checks** runs selector/gate unit tests and cheap ASCII/stale-slot/DB-pairing/file-size guards on eligible events; tidy/sqlc/build/vet/Ubuntu Go lint/mips64 lint are guarded by `heavy_allowed`. Windows shard regression tests run here alongside selector/gate and shard tests, including eligible cheap plans.
- **native-lint** runs golangci-lint on actual macOS and Windows runners when heavy work is authorized.
- **tests** calls `ci-tests.yml`: selected Ubuntu packages and race packages, Ubuntu normal agent batches, macOS native tests, selected Windows tests, and full Windows shards. Agent race runs on Ubuntu only; non-agent race packages are selected Ubuntu work. Both npm wrapper scripts run on Ubuntu, selected macOS native, and selected Windows or full Windows shard A. Linux process-group/lock-fence suites are a separate full-mode Ubuntu step.
- `test-native` builds its matrix from the plan: a macOS leg when Darwin is needed, a Windows leg only on a non-full plan (a full Windows plan runs in `windows-full-shards` alone). A step-level `exit 0` cannot skip a leg, which is why the old "no-op" step let a full plan run the whole Windows suite twice (about 100 minutes in `test-native` on top of the shards). `test-native` is capped at 75 minutes and `test-ubuntu` at 60.
- The macOS whole-agent step is gated by `matrix.os == macos-latest`, `macos_needed`, and (`mode == full` or the exact `github.com/PHPCraftdream/rush/internal/agent` token is in `darwin_packages`). Full main/main-bound `full-ci` selects all OSes. It uses eight sequential batches with `-short -failfast -parallel 2`; there is no macOS race, explicit timeout, or `-p` flag. Native-required prefixes require an actual Windows test receipt, not cross-compilation.
- **web**, **security**, **ktav-purity**, and **licenses** run their selected work. `ci-security.yml` includes PR dependency-review for full/dependency selections; GitHub Dependency graph is an external prerequisite. Ktav-purity and licenses support standalone triggers plus `workflow_call`; caller and standalone execution are independent. Licenses has fingerprint caching.
- **ci-gate** validates current live PR state, top-level needs, current-attempt jobs and expected leaves, assignment, plan identity, and required reusable jobs; API failure fails closed. Main-bound PR without `full-ci` fails with exactly `Full validation not requested for this revision.`

## Gate truth table

| Event/state | Work | Gate |
|---|---|---|
| Main-bound PR, full-ci absent (draft included) | minimal plan + cheap guards | red: `Full validation not requested for this revision.` |
| Main-bound PR, live full-ci present | promoted full selection | current source head/base + every full receipt required |
| Non-main PR | selector tier1/full fallback | live identity + selected receipts |
| Push main | promoted full selection | full receipts required |
| Unrelated label event | guarded plan/check/worker/cancel; gate ignores | no heavy work/cancellation |

## Windows assignment integrity

The gate verifies assignment and complete unique coverage against the selected Windows plan. Full Windows leaves include plan-shards, shard A-D, `windows-agent`, and summary. `windows-agent` is separate from shard A; npm runs on shard A. Native-required prefixes require an actual Windows test receipt, not cross-compilation.

## Cancellation and concurrency

Full/quick runs use distinct names and concurrency groups. Cancellation runs only on full-ci removal, re-reads live labels, and never proves gate eligibility. Ambiguous runs or API/fork denial are not cancelled and require operator attention.

Local Python unit tests, YAML parsing, actionlint and isolated helper tests do not prove live GitHub API behavior, runner job-name formatting, permissions or required-check configuration. Require an up-to-date merge candidate; changed base invalidates old results.

## Rollback and external configuration

Rollback is one revert of the eventual cutover commit; the exact hash is supplied by the orchestrator. Revert restores the five retired workflows (`build.yml`, `lint.yml`, `security.yml`, `ci-plan.yml`, `job-batches-shadow.yml`); these names are historical rollback references only. Release-type pushes remain in `snapshot.yml`; `lint-sync.yml` is manual and fork-skipped; `agent-leak-smoke.yml` is manual smoke.

## Existing plan outputs

| Output | Consumer |
|---|---|
| mode/per-OS package strings | test-ubuntu/native/fixed Windows shards |
| race_packages (agent excluded) | race-ubuntu |
| agent_selected | whole-agent job |
| web_changed | web lane |
| security_full/deps/actions | CodeQL Go/Actions, govulncheck, Grype, PR dependency-review |
| expected outputs, SHA identity, not_run | ci-gate |

`go mod tidy -diff` avoids mutation; Ubuntu and native lint are distinct real lanes; shortcode drift remains advisory. Mutation checking applies only to `mut/**`.

## Go cache layout (workflow configuration; not runtime-verified)

`ci.yml`, `ci-tests.yml`, and `ci-windows-shards.yml` disable `setup-go`'s built-in cache and explicitly cache paths from `go env GOMODCACHE` and `go env GOCACHE`. The module cache key is shared by jobs on the same OS and `go.sum`: `go-mod-${runner.os}-${hashFiles('go.sum')}`. Plan and summary jobs use the module cache only; test/build jobs also use the build cache.

Build-cache keys include OS, Go version, runner architecture, and a job discriminator, ending in the current commit SHA; restore prefixes omit the SHA to reuse the latest matching cache. Windows shard build caches are separately keyed by shard matrix value (A-D), in addition to OS/version/architecture, so shards don't share build entries. Job configuration intentionally distinguishes mixed work: the agent job combines race and non-race tests, while the checks job includes native and cross-target (mips64) work.

Only pushes to `main` save caches; other runs restore only. There is no extra cache-trimming step: Go self-trims old entries. Cache contents and actual CI runtime behavior have not yet been verified; the plan and summary above describe configured keys and paths only.
