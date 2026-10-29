# Build and test cost - measurements (2026-10-28)

Measured on `main` at `2e2ff0a4` (all repository packages, `-short`, one package at a time,
2 logical processors, cold Go build cache for the build numbers). The absolute numbers are
slower than a CI runner; the proportions are what matter.

## Cold build of every test binary (`go test -run '^$' -p 1 ./...`)

| Part | Time |
|---|---|
| Third-party dependencies (build) | 196 s |
| Standard library (build) | 37 s |
| This repository (build) | 104 s |
| Linking 61 test binaries | 65 s (about 1.1 s each on average, up to 3.6 s) |
| `go vet` run by `go test` | 52 s |
| Wall clock | 463 s |

Compilation of repository code is about a fifth of a cold build, and the build cache
(rolling since #261) covers the rest after the first run. Compile time is not what makes CI slow.

## Test run time (`go test -short -p 1 ./...`, 74 packages)

Total 2,470 s (41 min). Four packages are 78% of it:

| Package | Time | Share |
|---|---|---|
| `internal/app` | 878 s | 36% |
| `internal/config` | 395 s | 16% |
| `internal/cmd` | 355 s | 14% |
| `internal/agent` | 291 s | 12% |
| `internal/session` | 126 s | 5% |
| `internal/server` | 101 s | 4% |
| `internal/agent/tools/mcp` | 97 s | 4% |

Within them the time is concentrated in a few clusters of tests with nearly identical durations:

- `internal/config`: `TestOAuthRound7_CartesianMatrix` alone is about 160-200 s
  (roughly 40-50% of the package's 395 s).
  It is a 2112-case model check and runs under `-short`.
- `internal/app`: about ten `TestExecuteRunContinuation*` tests at about 25.6 s each, the
  `TestCLIQuota*` tests at 15-46 s, `TestRunTimeout*` at about 26-30 s.
- `internal/agent`: three `TestRun_QuotaLimit*` tests at 30.0 s each.

Identical durations point at a fixed wait (the tests contain no sleeps of that length; the
likely cause is the provider retry backoff that the test's failing stub triggers, since other
`internal/app` tests already lower the retry count). This is a hypothesis to verify, not a finding.

## Rebuild blast radius (from the action graph)

Touching a package rebuilds every importer and relinks every test binary that depends on it.
Upper bound, cold-equivalent cost: `internal/platform` 166 s (70 packages, 31 test binaries),
`internal/db` 164 s, `internal/session` 157 s (51 packages, 22 binaries), `internal/config`
137 s (41 packages, 18 binaries). The Go build cache cuts the cascade whenever a change leaves
the dependency's export data identical, so the real cost of a typical edit is far lower.

## Conclusions

1. Splitting heavy packages into sub-packages would shrink the rebuild cascade, not the test
   time, and compilation is already cached. With the measurements above it is not the best
   lever; do it only for a package where a concrete boundary exists without wide exports
   (see the rules in `CLAUDE.md` about sub-packages).
2. The lever is test run time: the longest single package is `internal/app` (878 s), so
   shortening its fixed waits shortens the critical path of every sharded run.
   Removing the fixed waits of the clusters above would cut about 500 s of the 2,470 s.
3. `TestOAuthRound7_CartesianMatrix` is a candidate for running outside `-short` (or a
   reduced `-short` subset) - an operator decision, because it is the only exhaustive check of
   the OAuth refresh arbitration.
4. `-count=1` in CI disables the Go test result cache. Dropping it would let unchanged
   packages skip their run entirely, at the price of masking flakes; also an operator decision.
5. A full local run catches what filtered runs miss: two regressions were found this way
   (the matrix since #262, and five batch tests since #245).

## Reproducing

```
go test -short -count=1 -p 1 -json ./... > tests.jsonl         # per-package / per-test elapsed
go test -run '^$' -p 1 -debug-actiongraph=ag.json ./...         # per-action build times
```

Run both under a memory cap (`capm 3g`) and `GOMAXPROCS=2`; keep the Go cache for the build
numbers in a throwaway `GOCACHE`.
