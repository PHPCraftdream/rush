# ci-structure — the tiered CI graph (shadow mode, phase 2)

Design record for the three new workflow files (`ci.yml`, `ci-tests.yml`,
`ci-security.yml`). Existing workflows (`build.yml`, `lint.yml`,
`security.yml`, `ci-windows-shards.yml`, `ci-plan.yml`) are untouched and
remain authoritative until cutover.

## Job graph

```
plan (ubuntu: ci_plan.py selector, 3 pre-extracted GOOS graphs)
├── checks (ALWAYS: guards + build/vet + golangci-lint + mips64 lint)
├── tests  (uses ./.github/workflows/ci-tests.yml)
│     ├── test-ubuntu          (plan's linux selection, build.yml group steps)
│     ├── agent-ubuntu         (8 sequential agent batches + one -race pass)
│     ├── race-ubuntu          (selected race pkgs, agent excluded)
│     ├── test-native          (matrix: darwin / windows tier1 selection)
│     └── windows-full-shards  (uses ./.github/workflows/ci-windows-shards.yml,
│                               only when mode == full && windows needed)
├── web     (lint.yml lint-web body + production build; only when web/ changed)
├── security (uses ./.github/workflows/ci-security.yml)
│     ├── codeql-go        (mode == full)
│     ├── codeql-actions   (.github/workflows/** changed)
│     ├── govulncheck      (full or deps changed)
│     └── grype            (full or deps changed)
└── ci-gate (always(): summary table + not_run list to $GITHUB_STEP_SUMMARY;
             fails on any failure/cancelled)
```

## What each job runs and why

- **plan** — checks out with `fetch-depth: 0`, computes `BASE_SHA`/`HEAD_SHA`
  (shadow: `git merge-base origin/ci/job-batches HEAD` vs HEAD; pull_request:
  `github.event.pull_request.base.sha` / `.head.sha`), pre-extracts the three
  per-OS `go list -deps -test -json ./...` graphs (same GOOS/GOARCH/CGO env as
  `ci_plan/golist.py`), runs `.github/scripts/ci_plan.py`, uploads the plan
  artifact, and exports job outputs from a python step reading `plan.json`.
- **checks** — the cheap guards, byte-for-byte from build.yml's ubuntu leg:
  SQL ASCII, stale slot spelling, DB release pairing + selftest, file-size
  ratchet + selftest, `go mod tidy -diff` (see deviations), sqlc drift,
  `go build ./...`, `go vet ./...`, pinned golangci-lint v2.13 + the
  GOOS=linux/GOARCH=mips64 ncruces-driver lint from lint.yml.
- **tests** → see ci-tests.yml header; every command/flag/timeout is copied
  from build.yml (`-short -failfast -p 2`, app `-timeout 20m`, Linux-only
  process-group suite without -short on full mode, agent 8×batches
  `-parallel 2`, race `-race -short -failfast -timeout 15m`, agent race as
  ONE `go test -race -short -parallel 2 -count=1 -timeout 60m
  ./internal/agent/` inside a 90m job).
- **web** — corepack + node 22 + `pnpm install --frozen-lockfile` +
  `pnpm typecheck` + `pnpm lint` + `node --test src/*.test.ts` (lint.yml
  verbatim) plus `pnpm build` (rsbuild production build; not in lint.yml —
  added here because the plan pays for web only when `web/` changed).
- **security** — CodeQL go/actions, govulncheck, Grype with the exact pins
  and permissions from security.yml, gated by plan flags. **dependency-review
  is NOT included** until the repo setting is enabled.
- **ci-gate** — renders the per-job result table and the plan's `not_run`
  list to the step summary; fails on any failure/cancelled. NOT REQUIRED YET:
  phase 2c hardens ci-gate (skip-vs-needed cross-check, parity audits). It is
  NOT added to any required checks / branch protection.

## How plan outputs map to jobs

| plan field / output | consumed by |
|---|---|
| `mode` | tests (`windows-full-shards` iff `full`; process-group suite iff `full`), `security_full` |
| `per_os.linux` → `linux_packages`, `ubuntu_needed` | test-ubuntu group resolution |
| `per_os.darwin` → `darwin_packages`, `macos_needed` | test-native macos leg |
| `per_os.windows` → `windows_packages`, `windows_needed` | test-native windows leg (tier1) |
| `race_packages` → `race_packages`, `race_needed` (agent excluded) | race-ubuntu group resolution |
| `internal/agent` in linux selection → `agent_selected` | agent-ubuntu |
| changed files → `web_changed` | web |
| changed files + mode → `security_full` / `security_deps` / `security_actions` | ci-security inputs |
| `not_run` (artifact) | ci-gate summary |

All gating is on precomputed boolean outputs (`ubuntu_needed == 'true'`
style); no `fromJSON` of huge strings anywhere — package lists travel as
plain space-separated string inputs/outputs.

## Shadow triggers vs cutover triggers

Current `ci.yml` (exactly what is on this branch):

```yaml
on:
  push:
    branches: ["ci-shadow/**"]
  workflow_dispatch:
```

Cutover replaces that block with (and ONLY that — every job body already
implements the pull_request base/head path):

```yaml
on:
  pull_request:
    types: [opened, reopened, synchronize, edited, ready_for_review, labeled, unlabeled]
  push:
    branches: [main]
```

No schedule/cron is added at cutover either (matches the fork's security.yml
cron removal).

## Deviations / awkward-reuse decisions (deliberate, not omissions)

- `go mod tidy -diff` replaces build.yml's `go mod tidy` + `git diff
  --exit-code` (same assertion, one process, no working-tree mutation).
- lint.yml's 3-OS golangci-lint matrix: only the ubuntu leg is reproduced in
  `checks`. The macOS/Windows legs are pure platform-formatting coverage
  (autocrlf/gofmt) with no selector meaning; they stay in lint.yml until
  cutover rather than paying 2 native runners on every shadow push.
- Native (darwin/windows tier1) legs run the build.yml group structure as one
  sequential bash loop with `::group::` markers instead of one named step per
  group; the ubuntu lane and ci-windows-shards keep the per-step split.
- build.yml's `shortcode-drift` job (`cahsync -check -strict`,
  `continue-on-error: true`) is NOT reproduced: it is advisory-only (warns),
  unrelated to the selector, and lint-sync/labeler-style jobs stay in their
  own workflows. Revisit at cutover if it should join `checks`.
- build.yml's test-race 5-batch agent split is replaced by the single
  whole-package invocation (mandated shape); the batching history lives in
  build.yml's comments and docs/lessons/2026-08-30-ci-goroutine-leak.md.

## NOT done (future work)

- ci-gate hardening (phase 2c): skip-without-declaration failure, required
  check wiring, branch protection — nothing added to branch protection here.
- `full-ci` label enforcement (labeler.yml exists; the plan does not read it).
- dependency-review: requires the repo setting to be enabled first.
- Replacing build.yml/lint.yml/security.yml triggers (the cutover task).

## Validate

```
git push origin <branch>:ci-shadow/<name>
```

Then watch the run in the Actions tab (workflow `ci`); ci-gate's step summary
has the result table and the not_run audit trail.

## Rollback

Delete `.github/workflows/ci.yml`, `.github/workflows/ci-tests.yml`,
`.github/workflows/ci-security.yml` and this file. Nothing else references
them; `ci-windows-shards.yml` keeps its own push trigger on `ci-shadow/**`.
