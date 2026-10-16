# CI selector replay — 2026-10-13 (Phase 1b validation)

Goal: replay 12 historical failure classes against the changed-package selector
(`.github/scripts/ci_plan.py`) and require that every plan for the change would
have run the (OS, package) where CI really failed.

A HIT requires the failing package in the per-OS selection AND that lane active
(linux lane always active; darwin/windows lanes active iff `native_required` or
mode full).

Range approximation: single-commit changes replayed as `<c>^..<c>`; PR merges
as `<merge>^1..<merge>` (true PR base). Package graph is the CURRENT tree's
`go list -deps -test -json ./...` (3 GOOS graphs, pre-extracted once) — an
approximation for older ranges.

## Results table (before → after policy fix)

Legend: l/d/w = number of packages selected on linux/darwin/windows after the
fix. Before and after counts are identical — only lane activation changed,
except where noted.

| id | change | real failure (OS, package) | plan.mode | selected l/d/w | failing pkg on failing OS | OS lanes | before | after | why (before) | fix |
|----|--------|----------------------------|-----------|----------------|---------------------------|----------|--------|-------|--------------|-----|
| 1 | 09300544 (process-group SIGKILL EPERM) | darwin, internal/agent | tier1 | 22/22/22 | included | darwin+windows | HIT (native via `_unix` suffix + native prefixes) | HIT | — | — |
| 2 | 85fa076f (windows rush.db in use, coordinator join) | windows, internal/agent | tier1 | 22/22/22 | included | none active | MISS | HIT | internal/agent not under native prefixes, no GOOS suffix → native_required=false → windows lane never ran | race_roots rule |
| 3 | 1b94fad9 (awaitSync select race) | darwin, internal/agent | tier1 | 22/22/22 | included | none active | MISS | HIT | same as 2 | race_roots rule |
| 4a | 8bbcc3d6 (shebang zombie wait) | darwin, internal/shell | tier1 | 1/1/1 | included | darwin+windows | HIT (internal/shell/ native prefix) | HIT | — | — |
| 4b | 09300544 (group-kill, internal/shell tests) | linux, internal/shell | tier1 | 22/22/22 | included | linux | HIT | HIT | — | — |
| 5 | aeac9807 (stall clock sampler race) | linux, internal/agent | tier1 | 22/22/22 | included | linux | HIT | HIT | — | — |
| 6 | c3d91f79 (busy-DB retry ordering) | linux, internal/agent | tier1 | 1/1/1 | included | linux | HIT | HIT | — | — |
| 7 | 952e9456 (heartbeat usage accounting) | windows, internal/agent | tier1 | 1/1/1 | included | none active | MISS | HIT | same as 2 | race_roots rule |
| 8 | 2704df6a (unjoined release callbacks) | darwin internal/server + windows internal/app (symptoms of an internal/agent change in OTHER packages on OTHER OSes) | tier1 | 22/22/22 | both included | none active | MISS | HIT | agent change selected internal/app+internal/server on darwin/windows but native_required=false so those lanes never ran | race_roots rule (agent is a race root → native lanes activate; reverse closure already carried app/server onto those lanes) |
| 9 | f3c3e21a (sdk close-admission vs shutdown, jb/flake-sdk-close) | darwin, sdk | tier1 | 1/1/1 | included | darwin+windows | HIT (sdk/ native prefix) | HIT | — | — |
| 10a | f7d1bf27 merge #14 (jb/r11b) | windows, internal/session | full | 70/27/27 | included | all | HIT (internal/session core+native) | HIT | — | — |
| 10b | 04e226b6 merge #17 (jb/r10c) | windows, internal/session | full | 70/27/27 | included | all | HIT | HIT | — | — |
| 11 | 41b55a06 (sqlc regenerate after R10c) | linux, internal/db (stale generated code) | full | 70/26/26 | included | all | HIT (internal/db core prefix → full; sqlc.yaml + internal/db/sql/*.sql + migrations map to internal/db; the drift guard lives in build.yml + pre-push and full mode carries it) | HIT | — | — |
| 12 | 4988d832 (awaitSync move, 1004-line violation) | linux, internal/agent | tier1 | 22/22/22 | included | linux | HIT for the package, but the file-size ratchet guard was NOT in the always-run set (`.githooks/` missing from always_run, and always_run was never consulted by the selector) | HIT (guard carried) | — | always_run wiring + `.githooks/` added |

## Fixes applied (principled, no commit ids hardcoded)

1. selector: any changed package (production OR test-only) under policy
   `race_roots` sets `native_required` — these are concurrency-sensitive
   packages whose flakes appear only under native scheduling (cases 2,3,7,8).
   Reverse-dependency closure already places native-required consumers
   (internal/app, internal/server) on those lanes; the rule activates the lanes.
2. policy: `always_run` now includes `.githooks/` (file-size ratchet
   `check_file_size.sh` + allowlist, `check_file_size_selftest.sh`, pre-push
   which mirrors CI incl. the sqlc ASCII/drift guard) (case 12, and guard-side
   of 11).
3. selector: `always_run` is now consulted — a change touching an always-run
   path adds a full-fallback reason, so guard-infrastructure changes never
   shrink the plan (cases 11, 12 guard side).

Unit tests added (`.github/scripts/test_ci_plan.py`):

- `test_race_root_change_requires_native_lanes`
- `test_race_root_change_runs_native_reverse_deps`
- `test_always_run_guard_change_forces_full`

Full suite: 17 tests OK.

## Cost / honesty

- After the fix, EVERY internal/agent change (including test-only, which is
  most agent PRs) activates darwin+windows lanes: a typical agent-only change
  now plans 22 packages × 3 OS lanes + the linux race suite, vs 22 packages on
  linux before. The module has ~70 linux packages, so tier1 agent plans still
  skip ~48 linux packages but pay 2 extra native lanes. 12 of 14 replay rows
  end in tier1 (2 in full); none ended in "all-native full" — no case required
  selecting everything, but the agent-package native activation is the real
  recurring cost and is judged necessary: 4 of the 12 historical failures
  (2,3,7,8) were agent-package flakes on macOS/Windows.
- Reverse closure for agent changes is wide (22 packages incl.
  app/server/cmd) — that width is what made case 8 land, so it is kept.
- Known approximations: single-commit ranges instead of true PR bases (except
  the two merges replayed as merge^1..merge); current-tree package graph
  applied to older diffs.

## Verdict

14/14 replay points HIT after the fix (4 were MISS before); no MISS was hidden — each was closed by a policy/algorithm change with a regression test; rollout of design §7 steps 2-4 is unblocked per this evidence.
