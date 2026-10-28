# ci-windows-shards (active reusable workflow)

Reusable Windows full-suite shards called by `ci-tests.yml`; this is not the removed shadow workflow. `.github/ci-shards/shards.toml` and this workflow define Windows group coverage. The gate/reusable-workflow callers reference this workflow, so rollback must not delete it.

## Shard map

| Job | Runs | Contents |
|---|---|---|
| windows-shard A | windows-latest | internal/app (-timeout 20m), internal/server + internal/session, sdk, npm wrapper cache tests, `go mod tidy` no-diff |
| windows-shard B | windows-latest | internal/cmd + internal/commands |
| windows-shard C | windows-latest | internal/agent subpackages, internal/shell, internal/jobbatch + cmd/batchlab |
| windows-shard D | windows-latest | permission/config/csync; db/message/queue/pubsub/history; oauth/deploy/update/version; computed catch-all |
| windows-agent | windows-latest | `internal/agent` exact package, 8 batches, -parallel 1, GOMEMLIMIT=1500MiB |

`plan-shards` checks exact-once package assignment and workflow group names against the manifest and live assignment. Planning and `shards-summary` both validate coverage.

## Timing summary

Test steps record start/end epochs in `timings.txt`; rebalance whole group entries while retaining max-parallel 2.

## Caching

Windows jobs explicitly restore a stable Go module cache and rolling per-shard Go build caches; cache saves run only on pushes to `main`.

## Rollback

Do not delete this reusable workflow: `ci-tests.yml` references it. Rollback of the CI cutover is a single revert of the eventual cutover commit, restoring retired entry-point workflows; use the exact commit hash supplied by the orchestrator.
