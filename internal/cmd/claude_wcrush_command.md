---
description: Delegate this task to a rush sub-agent that must work only inside an isolated git worktree and never run the tests — execution and verification move to a second, orchestrator-driven phase
---

<!-- NAME PROVISIONAL: the operator asked for /wcrush, but the sibling
     commands are /rush, /rush-fallback and /wrush — the name sits oddly
     in the fork's crush→rush convention. Task #922 pins the name into
     claude_init.go/claude_del.go paths, the multi-CLI converters and
     the tests; revisit it there before cementing `wcrush.md`. -->

This skill is **opt-in only** — invoked by typing `/wcrush <task>`.
**Do NOT auto-invoke on later turns.**

`/wcrush` is `/wrush` plus one hard rule: the sub-agent NEVER runs the
tests. **Before doing anything else, read the `wrush.md` file in this
same directory in full** — it defines the whole playbook `/wcrush`
inherits unchanged: the mandatory isolated worktree, refusal criteria,
rate-limit fallback, `--allow-peak-hours` rules, resuming
`awaiting_answer`, orchestrator mode, every `rush run` launching flag,
`--restrict-run`, monitoring/`sessions watch`, `sessions inject`,
stuck-lock recovery, and the zero-trust verification checklist. Only
the additions below change anything.

## Mandatory: two phases — the sub-agent never runs the tests

Two problems, one mechanism.

**Throughput.** On this machine the bottleneck for parallel agents is
not git, it is memory: two concurrent `go test -race` runs hit Windows
`ERROR_COMMITMENT_LIMIT` (errno=1455) and take each other down. The
thinking-and-writing phase costs almost nothing. So: N agents write
code in parallel; the orchestrator runs the suites one at a time.

**Safety.** This one is not theoretical. In the session that motivated
this command, a delegated agent hung mid-revert-check and left
`internal/agent/tools/mcp/init.go` deliberately broken — it had
injected a `lifecycleMu.Lock()` around a disk read to prove a test
could fail, and died before restoring it. Only worktree isolation kept
that out of the main checkout. An agent that never runs tests never
neuters production code to watch a test fail, so it cannot leave that
landmine at all.

### Phase 1 — write and statically verify, in parallel

The sub-agent writes the production code and the tests, and brings
them to "compiles and passes static analysis":

```
go build ./...
go vet ./...
gofmt -l .
```

plus the project's linter if it is cheap to run. These cost little
memory — running them is REQUIRED, not merely allowed. Handing over
code that does not compile defeats the whole arrangement.

**Phase 1 forbids:** `go test` in any form; revert-checks; debugging
by running anything; neutering production code to observe a failure.
All of that moves to phase 2.

### The handover — the final message is the contract

The agent ends its turn with a final assistant text message that
states plainly the work is **NOT verified by execution**, and lists:

1. **Which tests were written**, and what failure scenario each is
   meant to catch.
2. **The revert-check it intends for each** — precisely what to neuter
   so the test should fail, and what failure output is expected.
3. **What exactly needs to be run, and in what order.**

Without that list, phase 2 begins by reverse-engineering someone
else's intent.

### Phase 2 — on the orchestrator's permission, same session

The orchestrator resumes THE SAME session:

```
rush run --role smart --session <same id> "<permission and what to run>"
```

The conversation context is preserved, so the brief is not repeated.
Only now does the agent run the tests, perform the revert-checks, and
fix what fails.

**`--role` is mandatory when resuming** — omitting it exits
immediately with code 1. (Learned the hard way in the motivating
session.)

## Carried over from wrush.md, unchanged

Phase 2 moves who runs what, not any of wrush.md's rules — restated
here so neither side of the split can claim it didn't know:

- **The worktree mandate stands**: a dedicated worktree under
  `<repo-root>/worktrees/`, in-tree and gitignored, every invocation,
  solo or parallel. Never the primary checkout.
- **Never commit or push**; the orchestrator reviews the diff and
  merges.
- **OOM discipline belongs to phase 2** — the other half of the same
  bargain: `-parallel 2` for heavy packages, never two heavy runs at
  once, long runs backgrounded via the Bash tool's
  `run_in_background: true` parameter and never with a trailing `&`.
- **Zero trust survives the split.** The phase-1 handover and the
  phase-2 "tests pass" are CLAIMS, not receipts. The diff, the tests
  and the revert-checks get verified by the orchestrator personally —
  run one at a time, inside the worktree (wrush.md's checklist).
- **Do not touch** `CLAUDE.md`, `.github/workflows/`,
  `web/dist/.gitkeep`; no `ReportFindings` tool; no `code-review`
  skill.

**Not finished until the worktree is merged and removed** — wrush.md
steps 5 and 6, verbatim. An unmerged or un-removed worktree is
unfinished work, not a deliverable.

## Task

$ARGUMENTS
