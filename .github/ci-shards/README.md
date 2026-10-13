# ci-windows-shards (shadow workflow, Phase 2b)

Parallel-shard shadow of `.github/workflows/build.yml`'s Windows full suite.
build.yml is untouched; this workflow runs only on `ci-shadow/**` pushes or
via `workflow_call`.

## Shard map

| Job | Runs | Contents |
|---|---|---|
| windows-shard A | windows-latest | internal/app (-timeout 20m), internal/server + internal/session, sdk, npm wrapper cache tests, `go mod tidy` no-diff |
| windows-shard B | windows-latest | internal/cmd + internal/commands |
| windows-shard C | windows-latest | internal/agent subpackages (agentguard, cliprovider, codexprovider, hyper, limitwords, notify, prompt, tools — prefix so tools/mcp and mcp/internal/contextlock are covered), internal/shell, internal/jobbatch + cmd/batchlab |
| windows-shard D | windows-latest | permission/config/csync; db/message/queue/pubsub/history; oauth/deploy/update/version; + computed catch-all (every package not claimed elsewhere) |
| windows-agent | windows-latest | `internal/agent` exact package only, 8 batches, -parallel 1, GOMEMLIMIT=1500MiB (Windows branch of build.yml's test-agent) |

max-parallel: 2, fail-fast: false. `plan-shards` (ubuntu) runs
`assign_shards.py`, which exits nonzero unless every `go list ./...` package
is assigned exactly once — that IS the "not run" assertion, re-verified by
`shards-summary`. `shards-summary` also checks every build.yml Windows-leg
step name is mapped in shards.toml's `[windows_leg]` table and renders the
timing table (per-step wall seconds + approx total span) from the
`timings.txt` files in each `test-json-*` artifact.

## Validate

```
git push origin <branch>:ci-shadow/<name>
```

Then watch the run; the summary job's step summary has the timing table.

## Reading the timing summary

Each test step records start/end epochs into `timings.txt` (uploaded with
the `go test -json` logs). Rebalance by moving whole group entries between
shards in `shards.toml` so per-shard wall times equalize; keep max-parallel
at 2.

## Caching

No `actions/cache` is pinned anywhere in this repo, so none is added:
caching is actions/setup-go's built-in module+build cache. Its hit/miss is
visible only in the setup-go step log; the summary reports setup-go step
duration so cold-vs-warm restore time can be compared across runs.

## Action pinning

All actions are pinned by SHA. `actions/upload-artifact` is pinned to the v4.6.2
commit and `actions/download-artifact` to the v4.3.0 commit; both SHAs were
resolved with `gh api repos/actions/<name>/git/ref/tags/<tag>` (object type
`commit`) by the orchestrator.

## Rollback

Delete `.github/workflows/ci-windows-shards.yml`. Nothing else references
it; build.yml is untouched.

## NOT done (future work)

- Replacing build.yml's Windows leg with these shards is the cutover task.
- No pull_request trigger, no schedule/cron.
- Ubuntu/macOS legs untouched (including the Linux-only guard/-race jobs).
