# Worktree delegation: `rush run --worktree` and `rush worktree`

`rush run --worktree <slug> [--base <ref>] [--branch <name>]` replaces
the manual chain: `git worktree add -b <slug> worktrees/<slug> <base>`,
`cd`, `rush run ... < prompt > main/.rush/stdin/<s>.out 2> .err`, then a
manual `git worktree remove` plus branch cleanup that on Windows
routinely leaves locked directories behind.

## Quick start

```bash
rush run --role smart --worktree "task-a" --json "refactor the parser" \
  > /tmp/out.json 2>/tmp/out.err
jq -r '.exit_reason' /tmp/out.json
rush worktree list
rush worktree remove "task-a"
```

The redirection is optional: stdout (the `--json` envelope, or the terse
final answer) and everything on stderr are additionally mirrored into
`<main>/.rush/stdin/task-a.{out,err}`.

## Semantics

- **Creation / reuse** — the main checkout is resolved via git (works
  when rush itself runs inside a linked worktree); the worktree lives at
  `<main>/worktrees/<slug>`. Branch `<branch|slug>` is created from
  `--base` (default: the main checkout's HEAD); an existing local branch
  is checked out instead of created (`--base` then ignored with a
  note). A registered worktree whose branch matches is reused
  (`reusing worktree <path> (branch <b>)` on stderr); an occupied path,
  a detached worktree, or a branch mismatch (naming both branches and
  suggesting `--branch`) are clean errors.
- **Slug rules** — must match `[a-z0-9][a-z0-9._-]*`: no uppercase, no
  leading punctuation, no separators, no `..`; Windows reserved device
  names (`con`, `prn`, `aux`, `nul`, `com1-9`, `lpt1-9`) are rejected.
  A worktree can therefore only ever be created under
  `<main>/worktrees/`.
- **Session default** — without `--session`, the session id is the
  slug. Data lives in the main checkout's shared `<main>/.rush`
  (production binaries; dev builds isolate to `<wt>/.rush/dev`).
- **Mirroring** — the run's stdout and stderr are also written to
  `<main>/.rush/stdin/<session>.{out,err}`, each capped at the last
  8 MiB, so results survive `rush worktree remove`. Shell redirection
  keeps working exactly as before.
- **Ignore warning** — if `worktrees/` is not git-ignored (`git
  check-ignore`), a warning is printed to stderr; rush never edits
  `.gitignore` itself.
- **Flag conflicts** — incompatible with `--cwd` and `--data-dir`;
  `--base`/`--branch` without `--worktree` is an error. All other run
  flags work unchanged.

## Removal contract

`rush worktree remove <slug> [--keep-branch] [--force]` (see
`rush worktree remove --help`):

- Refuses while a live run holds a session lock for this worktree (the
  slug-named session in `<main>/.rush`, or any lock in
  `<wt>/.rush/locks`) — **even with `--force`**; stop the run first
  (`rush sessions kill <id>`).
- Refuses a dirty worktree (uncommitted or untracked changes) without
  `--force`.
- Branch policy: default deletes with `git branch -d` (merged only — an
  unmerged branch is kept with a stderr notice, exit 0); `--force`
  deletes with `-D` (unmerged commits discarded); `--keep-branch` skips
  deletion; detached worktrees have no branch to delete.
- Windows: removal retries while AV/indexers hold the directory; if a
  process still holds it, the command honestly reports what remains on
  disk instead of claiming success.

`rush worktree list` prints a SLUG / BRANCH / SESSION / PATH table for
every registered worktree directly under `<main>/worktrees`.

## Non-goal

This is git-level isolation of the workspace, **not** an OS sandbox —
see [README: Security — `rush run`
permissions](../README.md#security--rush-run-permissions).

Full flag reference: `rush run --help` (Git-worktree isolation section).
