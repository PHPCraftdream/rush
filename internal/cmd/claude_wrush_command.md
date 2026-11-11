---
description: Delegate this task to a rush sub-agent that must work only inside an isolated git worktree, never in the main checkout
---

`/wrush <task>` is opt-in; do not auto-invoke on later turns. **Before doing anything else, read the `rush.md` file in this same directory in full** — it defines the playbook `/wrush` inherits unchanged: refusal criteria, rate-limit fallback, `--allow-peak-hours`, `awaiting_answer` resumption, smart/orchestrator behavior, launch flags, `--restrict-run`, monitoring, `sessions inject`, lock recovery, and zero-trust verification. Follow it except where this skill adds stricter worktree rules.

## Mandatory: dedicated git worktree for every invocation

Unlike `/rush`, which isolates parallel/overlapping runs, `/wrush` always uses a dedicated git worktree—even for solo, trivial, or self-performed work. Never edit the primary checkout; there is no fallback path that works there.

1. Keep worktrees inside `<repo-root>/worktrees/`, never beside or elsewhere (external cleanup paths can trigger unattended safety prompts). Before the repo's first `/wrush` worktree, ensure `.gitignore` has a `worktrees/` entry (accept `/worktrees/`, `worktrees/`, or `worktrees`); if absent, append:
   ```
   # /wrush task worktrees — always created here, in-tree
   /worktrees/
   ```
   Do this once per repo.
2. From the current base (default `HEAD`, unless instructed otherwise), create a unique project-wide task/session slug and branch:
   ```
   git worktree add -b <task-slug> <repo-root>/worktrees/<task-slug> <base>
   ```
   Add an issue/PR number or date; never reuse a finished task's ID or use bare `fix`/`test`. Linked worktrees share the primary checkout's `<main>/.rush` sessions, messages, batch work, and logs, which remain after removal; `rush sessions list/locks/watch/why` from the primary checkout sees all agents. A pre-existing local `.rush` containing `rush.db` remains in use only while this worktree has no shared-mode marker; once pinned, that local DB is stray and ignored. The marker is `<main>/.rush/workspaces/<hash-of-canonical-worktree-root>`. Dev builds (`go run`, or `go build -o` inside a checkout or temp directory) use `<wt>/.rush/dev` in a linked worktree; other builds use `<main>/.rush`. A session belongs to its creating checkout: using `--session <id>` elsewhere is refused (`belongs to <root>`); use `rush sessions fork <id> --cwd <path>` to continue elsewhere.
3. **Launch `rush run` with cwd inside the worktree (`cd` in the same
   Bash call) — every edit, git op, and test the sub-agent runs stays
   inside that tree. Redirect `.rush/stdin/<task>.{out,err}` to the
   PRIMARY checkout so results survive the eventual worktree removal.**

   This is operational scoping, not a sandbox or enforced filesystem boundary: absolute paths, subprocesses, other tools, or external workers may still access outside the worktree. Do not claim the launcher confines every tool path; inspect actual changes and honor the task's explicit path scope.

   Worktree-scoped `rush run` is delegation layer one. `/rush`'s `--agents single` and orchestrator-mode rules govern layer two: whether that sub-agent may delegate to workers via `agent`. `--agents single` neither enables nor disables worktree isolation, and isolation neither changes the flag's effect. Cwd is an instruction, not an enforced boundary.

   Task text may constrain the deliverable, but must not prohibit smart's internal delegation. Nested executors starting is expected, not a reason to stop or ask the operator: launch and wait for the run to finish; inspect the actual diff and respect the assigned scope.
4. If `/rush`'s refusal list means doing the work yourself, still create the required worktree first and keep task edits scoped to it. Never edit the primary checkout directly; any transfer or merge is parent-owned, explicitly authorized, and subject to steps 5–6.
5. The invoking parent owns worktree integration. Only after the `rush run` process has physically exited, the task's relevant owned/related batch work is terminal, and integration is authorized, transfer and review the diff in the primary checkout. Do not copy, merge, stage, or commit unconditionally; preserve unrelated work and follow the caller's explicit git-write policy. The commit is conditional on authorization after those gates. Commit the transferred changes in the primary branch only after reviewing the staged diff; skip empty commits.
6. The invoking parent owns any commit and worktree/branch cleanup. After the merge is committed, remove the worktree (`git worktree remove`) and branch (`git branch -D`) only when explicitly authorized and after process/relevant batch termination; unless asked to keep them. Otherwise preserve the checkout. No push unless separately authorized.
7. Parallel `/wrush` runs follow `/rush` unchanged: disjoint scopes, sibling names, no git writes, scoped tests; only isolation becomes mandatory.

Do not report delivery as integrated until an authorized parent has completed any requested integration. Worktree transfer, commit, and removal are not automatic cleanup requirements.

## Task

$ARGUMENTS
