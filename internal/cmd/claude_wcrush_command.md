---
description: Delegate this task to a rush sub-agent that must work only inside an isolated git worktree and never run the tests — execution and verification move to a second, orchestrator-driven phase
---

`/wcrush <task>` is opt-in; do not auto-invoke on later turns. **Before doing anything else, read the `wrush.md` file in this same directory in full** — `/wcrush` inherits its worktree, refusal/fallback, `--allow-peak-hours`, `awaiting_answer`, smart/orchestrator, launch flags, monitoring, `sessions inject`, lock recovery, and verification/merge/cleanup rules. Follow it except where this skill assigns testing to phase 2.

## Mandatory: two phases — the sub-agent does not run tests

**Why:** Concurrent `go test -race` runs have hit Windows `ERROR_COMMITMENT_LIMIT` (errno=1455), while writing costs little memory. Also, an agent previously hung during a revert-check after injecting `lifecycleMu.Lock()` around a disk read in `internal/agent/tools/mcp/init.go`, leaving production code broken. Worktree isolation kept it out of the primary checkout. Keeping execution/revert-checks out of phase 1 prevents this kind of test-induced landmine.

### Phase 1 — write and static checks, in parallel

The sub-agent writes production code and tests. It follows the caller's verification policy: do not run local checks when CI-only execution is requested, report CI-only status, and list exact CI commands for the parent; otherwise run only explicitly authorized checks. No execution is implicitly authorized by `/wcrush`.

Hand over code without claiming execution checks that were not authorized or run. State clearly what was and was not verified, and list the tests written with the failure scenarios they are intended to catch. Where caller policy is CI-only, provide the exact CI commands for the parent without claiming execution or adding revert-check requirements.

### Handover — final message contract

The agent's final assistant message must state what verification policy applied and plainly identify anything **NOT verified by execution**, then list:

1. Tests written and the failure scenario each is intended to catch.
2. What checks were run, or the exact CI commands required by the caller's policy (not run locally when CI-only).
3. Any known limitations or assumptions.

Without this, phase 2 must reverse-engineer the intent.

### Phase 2 — with orchestrator permission, same session

Resume the **same** session from the worktree that created it, only after the original physical `rush run` process has exited and all owned batch work is terminal. A session belongs to its creating checkout, so run this command with the worktree as the current directory; do not resume it from the primary checkout. A child paused on a question is not an exited top-level run; use `/rush`'s legacy-child or held-batch-question route as appropriate. This phase is only for verification the caller authorizes; it does not override CI-only policy.

```
rush run --role smart --session <same id> "<permission and what to run>"
```

Context is preserved. Run only checks authorized by the caller's policy; never presume phase-2 execution permission. `--role` is mandatory when resuming; omitting it exits with code 1.

**Reviewer auto-pass:** with a reviewer configured, this successful `--role smart` resume adds a Reviewer turn reviewing the complete phase-1+phase-2 session. Its conclusion is carried in the JSON envelope's additive `review` field; the executor's answer remains `final_text`. This is Rush-driven, not extra requested work; unconfigure the reviewer to disable it.

## Inherited rules that phase split changes

- **Delegation stays enabled:** task text may constrain the deliverable, but must not prohibit smart's internal delegation. Nested executors starting is expected, not a reason to stop or ask the operator: launch and wait for the run to finish.
- **Verification follows caller policy** — CI-only requests remain CI-only; `/wcrush` does not require local build, vet, formatting, or test commands.
- **Phase-2 verification follows caller policy** — run long phase-2 tests only when explicitly authorized; a CI-only request means do not run them locally. When execution is authorized, honor project memory and concurrency limits and wait for every started process to finish; never detach a child with `&`.
- **Zero trust:** handover reports are claims, not receipts. The parent personally reviews the diff and tests against the caller's verification policy, one at a time, inside the worktree using wrush.md's checklist.
- Do not touch `CLAUDE.md`, `.github/workflows/`, or `web/dist/.gitkeep`; do not use `ReportFindings` or the `code-review` skill.

**`/wrush`'s dedicated git worktree and no-sub-agent-commit/push rule remain in force. The invoking parent owns transfer, commit, and cleanup, only when authorized and after physical process plus relevant batch termination. Do not report integration complete until any explicitly requested, authorized transfer is complete; do not commit or remove the worktree unconditionally. When authorized after these gates, the parent commits only those changes before removing the worktree.**

## Task

$ARGUMENTS
