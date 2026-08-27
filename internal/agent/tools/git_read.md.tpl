Run a fixed, closed set of READ-ONLY git subcommands in the repository containing the working directory. The repository cannot be modified: only the subcommands listed below can run, and each is built from a fixed argv template — this is not a restricted shell, just a fixed argv builder, so checkout/reset/commit/push/stash and friends are impossible.

<operations>
- status: working-tree and branch state. Example: `git status --porcelain=v1 --branch`
- diff: unstaged changes (or staged against HEAD with `staged: true`). Example: `git diff` / `git diff --cached -U5 -- src/main.go`
- log: commit history (default 20, hard cap {{ .MaxLogCommits }} — larger max_count values are clamped). Example: `git log -n 20 -p -- src/`
- show: one commit/tree object; `ref` is required. Example: `git show HEAD -- README.md`
- blame: per-line attribution; `path` is required. Example: `git blame HEAD -- src/main.go`
- branch_list: local branches with tracking info; `include_remote: true` adds `-a`. Example: `git branch --list -vv -a`
</operations>

<behavior_notes>
- Runs with `git`'s working directory set to the working directory; git resolves the repo root itself.
- `path` must be relative to the working directory and must stay inside it; anything resolving outside is rejected outright (no permission is requested).
- `ref` and `path` values starting with '-' are rejected before git runs.
- Output is truncated at {{ .MaxOutputLen }} characters; each invocation is bounded by a {{ .TimeoutSeconds }}-second timeout.
- Concurrent-safe: safe to run from multiple subdirectories of the same repo simultaneously.
</behavior_notes>
