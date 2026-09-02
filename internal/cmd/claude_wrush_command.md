---
description: Delegate this task to a rush sub-agent that must work only inside an isolated git worktree, never in the main checkout
---

This skill is **opt-in only** — invoked by typing `/wrush <task>`.
**Do NOT auto-invoke on later turns.**

`/wrush` is `/rush` plus one hard rule: the task is ALWAYS solved
inside a dedicated git worktree, never the primary checkout. **Before
doing anything else, read the `rush.md` file in this same directory in
full** — it defines the whole playbook `/wrush` inherits unchanged:
refusal criteria, rate-limit fallback, `--allow-peak-hours` rules,
resuming `awaiting_answer`, orchestrator mode, every `rush run`
launching flag, `--restrict-run`, monitoring/`sessions watch`,
`sessions inject`, stuck-lock recovery, and the zero-trust verification
checklist. Only the additions below change anything.

## Mandatory: an isolated worktree, every time

`/rush` only worktree-isolates parallel/overlapping runs; `/wrush`
makes it unconditional — every invocation, solo or parallel, trivial
or not, gets its own worktree. No fallback path ever touches the
primary checkout.

1. **Worktrees live INSIDE the repo, under `<repo-root>/worktrees/`.**
   Never a sibling or otherwise external directory — an external path
   (e.g. `../repo-wt/<slug>` or a bare `/tmp`-style path) looks
   unrelated/unexpected to safety tooling, so cleanup commands against
   it (`rm -rf`, `git worktree remove --force`) get flagged as
   dangerous and stall the run on a confirmation prompt nobody is
   there to answer. A path the repo itself owns and ignores does not
   trip that.

   Before the FIRST `/wrush` worktree in a repo, check `.gitignore` for
   a `worktrees/` entry (any form — `/worktrees/`, `worktrees/`,
   `worktrees`) and append one if missing:
   ```
   # /wrush task worktrees — always created here, in-tree
   /worktrees/
   ```
   Do this once per repo, not on every invocation.

2. Create a branch + worktree from the current base (default `HEAD`,
   unless told otherwise): `git worktree add -b <task-slug>
   <repo-root>/worktrees/<task-slug> <base>`. Name `<task-slug>` like a
   `--session` id.
3. Launch `rush run` with cwd inside the worktree (`cd` in the same
   Bash call) — every edit, git op, and test the sub-agent runs stays
   inside that tree. Redirect `.rush/stdin/<task>.{out,err}` to the
   PRIMARY checkout so results survive the eventual worktree removal.
4. **Doing it yourself instead of delegating** (rush.md's refusal
   list) still means: worktree first, edits there, verify and merge
   like a sub-agent's diff. Never edit the primary checkout directly.
5. Once verified (rush.md's checklist, run inside the worktree),
   **merge the reviewed diff into the primary checkout yourself** —
   `cp`/`Edit` for non-overlapping files, hand-reconcile when a
   concurrent `/wrush` task touched the same file. Prefer this over
   `git merge`/`git pull` unless the user explicitly asks for that.
6. **Remove the worktree and branch** (`git worktree remove`, `git
   branch -D`) once the merge is committed, unless told to keep it.
   Since the worktree lives under `<repo-root>/worktrees/`, this is
   always a same-repo, ignored-directory cleanup — never a path outside
   the repo.
7. Parallel `/wrush` tasks follow rush.md's parallel-run rules
   unchanged (disjoint scope, sibling names, no git writes, scoped
   tests) — isolation is just compulsory instead of conditional now.

**Not finished until steps 5 and 6 both happened.** An unmerged or
un-removed worktree is unfinished work, not a deliverable — it won't
show up in the branch the user is looking at, and it litters `git
worktree list` for the next session. Don't report done, and don't
stop, until you've personally merged and removed it — never delegated
or deferred.

## Task

$ARGUMENTS
