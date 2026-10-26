# CI architecture and cutover

`.github/workflows/ci.yml` is the active tiered CI entry point for pull requests targeting `main`, pushes to `main` or the retained shadow branches, and manual dispatch. This branch contains merged `81b3bf63` changes, including `internal/ktavcodec` and the `tools` license checker; there is no blocked merge. No Git writes or workflow runs are part of this documentation update.

## Active lanes

- `plan` validates base/head identity, selects work from `.github/ci-policy.toml`, and publishes a plan artifact. PR base/head come from the event; main pushes use before/SHA; dispatch and shadow pushes use the `origin/main` merge-base.
- `checks` runs selector/gate unit tests and cheap guards on eligible events; heavy-authorized runs add tidy, sqlc consistency, compilation, vet, Ubuntu Go lint, mips64 ncruces lint, and advisory shortcode drift.
- `native-lint` runs the actual golangci-lint matrix on macOS and Windows when heavy work is authorized. Native-required Windows prefixes require a real Windows test receipt, through selected packages or the full Windows shard; cross-compilation is not a substitute.
- `tests` calls `ci-tests.yml` for selected Ubuntu tests, macOS native tests, Windows native tests or full Windows shards, agent normal/race, package race, npm wrapper cache tests, and Linux process-group/lock-fence suites. Full Windows work uses `ci-windows-shards.yml`.
- The macOS whole-agent step runs only when `matrix.os == macos-latest`, `macos_needed`, and (`mode == full` or the exact `github.com/PHPCraftdream/rush/internal/agent` token is in `darwin_packages`). `.github/ci-policy.toml` `native_required_prefixes` governs native lanes; unrelated tier1 scopes do not force this heavy agent step. Full main/main-bound `full-ci` selects all OSes. It extracts test names into eight sequential batches using `-short -failfast -parallel 2`; there is no macOS race, explicit timeout, or `-p` flag.
- npm runs both wrapper scripts on Ubuntu `test-ubuntu`, selected macOS native, and Windows tier1 native/full shard A. Windows shard regression tests run alongside selector/gate and shard tests in checks, including eligible cheap plans. Linux process-group/lock-fence tests are a separate full-mode Ubuntu step.
- `web` installs dependencies, typechecks, lints, runs unit tests, and builds when selected.
- `security` calls `ci-security.yml`: CodeQL Go/Actions, govulncheck and Grype as selected; dependency-review is retained for PR full/dependency selections. GitHub Dependency graph must be enabled externally.
- `ktav-purity` and `licenses` each support standalone triggers as well as `workflow_call`. The main workflow calls them on its heavy-authorized lane; standalone runs are independent. License fingerprint caching can skip a repeated scan. The independent standalone trigger plus caller invocation is intentional and retained from main.
- `ci-gate` validates live PR labels/head/base, top-level results, exact current-attempt receipts, and required reusable-workflow results; it fails closed on missing/API-failed receipts or absent required work. For a main-bound PR without `full-ci`, the exact gate failure is `Full validation not requested for this revision.`
- `cancel-full-on-unlabel` performs cancellation only; it cannot authorize the gate.

## Windows shard coverage

`.github/workflows/ci-windows-shards.yml` and `.github/ci-shards/shards.toml` define full Windows package groups. Coverage checks compare workflow group names with the manifest and live assignment. Exact-once package assignment is checked separately during planning and summary.

## Previous check mapping

These are historical names for mapping old required checks, not active workflows:

| Previous check/workflow | Active replacement |
|---|---|
| Compilation | `ci.yml` checks |
| Package tests | `ci-tests.yml`: selected Ubuntu/native packages and full Windows shards |
| Agent normal | `agent-ubuntu`; batched macOS `test-native` when gated; separate Windows `windows-agent` |
| Agent race | Ubuntu whole-package race step in `agent-ubuntu` only |
| Non-agent race | Selected Ubuntu packages in `race-ubuntu` |
| Tidy, sqlc, SQL ASCII, stale-slot, DB pairing, file-size, and self-tests | `ci.yml` checks |
| Shortcode drift | Advisory `ci.yml` checks step (`continue-on-error`) |
| npm wrapper cache tests | Both scripts: Ubuntu, selected native macOS, and selected Windows or full Windows shard A |
| Linux process-group/lock-fence suites | Separate full-mode Ubuntu `ci-tests.yml` step |
| Go lint | Ubuntu `ci.yml` checks and macOS/Windows `native-lint` |
| Web lint | `ci.yml` web lane |
| ncruces driver lint | mips64 `ci.yml` checks lane |
| CodeQL, govulncheck, Grype | `ci-security.yml` security lane |
| Dependency review | `ci-security.yml` security lane for PR full/dependency selections |
| Ktav purity and license checks | Standalone workflows and `ci.yml` reusable calls |
| Windows shard regression | `.github/ci-shards` tests in `ci.yml` checks, alongside selector/gate tests |

## Selection and required checks

`ci-gate` is the single aggregate verdict for CI. The `full-ci` label requests promoted full validation; without it the gate reports that full validation was not requested for this revision. Drafts do not bypass this rule. Main pushes are full; other PRs use selector tier1/full-fallback work. The `ci.yml` pull-request trigger is limited to base branch `main`. The `main` branch has no branch protection or rulesets, so there is no required-check switch or compatibility gate to configure. Open PRs receive the new CI on their next push. The hosted check name is not asserted here.

The duplicate shadow selector is retired; selector/gate unit tests are now checks. The obsolete integration shadow has been deleted. Mutation-check (`mut/**`) remains, with no PR overlap. The historical mapping for dependency-review distinguishes the current main workflow, which has no dependency-review job, from predecessor `3eb`, which did; retaining it preserves that original requirement. `full-ci` instructions above describe requested work, not a guarantee that label guards will permit every run.

## Rollback and separate workflows

Rollback is one revert of the eventual cutover commit; the exact commit hash must be supplied by the orchestrator. That revert restores the five retired workflow files: `build.yml`, `lint.yml`, `security.yml`, `ci-plan.yml`, and `job-batches-shadow.yml`. These names are historical rollback references, not active workflows. Release-type pushes on main remain in `snapshot.yml`; `lint-sync.yml` remains manual and fork-skipped; `agent-leak-smoke.yml` remains manual smoke. `snapshot.yml` is upstream-only release behavior. Dependency graph enablement is external. Documentation and local static checks do not prove hosted GitHub behavior.
