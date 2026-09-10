# Rush — Fork-Specific Merge & Development Guide

This file is **fork-local guidance** (not committed upstream). The fork
(`PHPCraftdream/rush`) has diverged substantially from
`charmbracelet/crush` and is **NOT a passive mirror**. Treat upstream as
an external project whose changes are imported selectively, never as a
source of truth.

## The Golden Rule

**Fork changes always win.** When upstream and fork disagree, the fork's
version is correct by default. Upstream changes are imported only when:

1. They fix a real bug that also affects the fork, AND
2. They do not regress fork-specific functionality, AND
3. They do not touch a subsystem the fork has rewritten or removed.

If any of those fails, **drop the upstream commit** with a one-line note
in the merge commit (`SKIP <hash>: <reason>`).

## Review Stop Rule — When a Review Series Ends

Reviews of this repository MUST have a termination condition. Without one
they do not converge: reviewing concurrent code over shared mutable state
always yields one more reachable interleaving, and every guard added to
close one widens the state space for the next. That is not hypothetical —
it is what happened to `internal/agent/tools/mcp/init.go` between
2026-09-02 and 2026-09-09: 683 lines held steady for two weeks, then grew
to 6478 across ~150 `fix:` commits in one week, while still producing
fresh P0 defects. See `docs/plans/2026-09-09-mcp-consolidation-plan.md`.

The rule, for every review cycle:

- **P0/P1 with a reproducible failure scenario** — fix in the same cycle.
  "Reproducible" means a concrete input/state that produces a wrong result,
  a crash, or a hang; not "this interleaving looks unprotected".
- **P2/P3** — record in the backlog, **do NOT fix in the same cycle**.
  They are inputs to a future planned piece of work, not to this one.
- **A review that finds no P0/P1 ends the series.** Do not schedule the
  next round to confirm the absence.

A follow-up round is justified only by a new P0/P1, a new subsystem, or an
explicit operator request — never by "the previous review's P3 list is
still open".

## No Source File Over 1000 Lines

A hand-written `.go`, `.ts` or `.tsx` file over 1000 lines gets
decomposed. This is the operator's rule, and it is backed by checks
because the informal version of it decayed — `init.go` above is what
that looks like.

Three layers enforce it:

- `.githooks/check_file_size.sh`, run by `pre-push`, over every tracked
  `.go`/`.ts`/`.tsx` file. Generated files are skipped by their
  `Code generated … DO NOT EDIT` marker, not by name.
- `revive`'s `file-length-limit` in `.golangci.yml`, at the same 1000
  with `skipComments`/`skipBlankLines` both false so the count matches
  `wc -l`.
- `max-lines` in `web/.oxlintrc.json` for the TypeScript side.

`.githooks/file_size_allowlist.txt` is a **ratchet, not an exemption
list**: a listed file may stay over the limit but fails the moment it
grows, and fails just as loudly once it drops to the limit and is still
listed. The allowlist is the single source of truth; the `.golangci.yml`
exclusions are derived from it, and `check_file_size_selftest.sh`
asserts that bijection in both directions. When you decompose a file,
remove it from **both** lists in the same commit.

**"Into a folder" means files in the same package directory, never a
sub-package.** Go treats a directory as one package, so moving a
declaration between files in it changes nothing — no visibility change,
no import change, no initialization-order change. A sub-package would
force exporting whatever crosses the new boundary, which for the MCP
package means its twelve package-level variables: the globals migration
this fork examined and rejected, arrived at through the back door.

Prove a split is a pure move before believing it:

```bash
git show HEAD:<old>.go | grep -hE '^(func|type|var|const) ' | sort > before.txt
cat <new...>.go        | grep -hE '^(func|type|var|const) ' | sort > after.txt
diff before.txt after.txt          # MUST be empty
```

For TypeScript the equivalent oracle is the sorted list of exported
names. For test files, also check the test-function count is unchanged.

## The MCP Subsystem — Read the Registry First

`internal/agent/tools/mcp` is the fork's most defect-dense subsystem and
the one where reviews stopped converging. Before changing anything in
it, read `docs/mcp-invariants.md`: 25 invariants, each anchored to the
code that enforces it and mapped to the tests that would catch its
violation. The map is maintained — the ten-file split re-anchored every
row in the same commit that moved the code, because a registry that lies
about where its law lives is worse than no registry.

Two conclusions from that work, so they are not re-litigated:

- **The package's globals are not migrating to `Owner`.** It was costed:
  `docs/plans/mcp-ownership-design.md` measured the full migration's
  benefit at 33 lines, because the identity branches degenerate into
  closing-fences rather than disappearing. 20 of the 25 invariants are
  mechanical — they constrain ordering within one owner and survive
  per-owner state unchanged, so the migration would not have prevented
  either P0 this subsystem actually produced.
- **Multi-`App` isolation is an API problem, not a state problem.**
  It is solved by the two-tier surface (methods on `*Owner`, package
  functions delegating to the process-current one) plus standalone
  owners, not by moving state.

## What This Fork IS — Repositioned Identity

The fork is positioned as **agent-tooling**: a CLI optimised for
delegation from Claude Code / orchestrators / scripts. The crown jewels
are:

- `rush run` non-interactive entry point + agent harness
- `sessions` subcommand family (pick / watch / tree / kill / locks /
  purge / reap / gc / grep / diff / cost / fork)
- `cliprovider` — invoke local `claude` / `gemini` / `codex` / `qwen`
  CLIs as fantasy.Provider
- `agentguard` — block sub-agents from touching shared git/state
- `agentic_fetch_tool`, `hooked_tool`, `loop_detection`,
  `stream_watchdog`, `usage_fallback`, `notify`
- `claude-init` / `claude-del` — install slash-commands + sub-agents
  into `.claude/{commands,agents}/`
- Per-model atoms + short codes (`fl`, `ox`, `s46xx`, …) in
  `cmd/models_atoms.go`
- React web UI under `web/` (Playwright e2e) — replaces upstream TUI
- Pre-push hook mirroring CI under `.githooks/pre-push`
- `/wcrush` — the two-phase delegation contract (see below)

## `/wcrush` — Write in Parallel, Verify in Series

`/wcrush` is `/wrush` plus one hard rule: **the sub-agent never runs
tests.** Phase 1 writes the code and proves it compiles — `go build`,
`go vet`, `gofmt` are required, not optional — then hands over with a
written list of what it wrote, what each test is meant to catch, and the
revert-check it intends for each. Phase 2 resumes the *same* session
(`rush run --role smart --session <id>`; omitting `--role` exits 1
immediately) once the orchestrator authorises the runs.

Two reasons, and the second is not theoretical:

- **Memory.** Two concurrent `go test -race` runs on this machine hit
  Windows `ERROR_COMMITMENT_LIMIT` (`errno=1455`) and take each other
  down. Thinking and writing cost almost nothing; the suites are the
  expensive part, so they serialise. Note this bites for heavy
  *compilation* too, not just tests — two agents each driving builds
  over a large package is enough.
- **Safety.** An agent that never runs tests never neuters production
  code to watch one fail, so it cannot die mid-revert-check and leave
  the neutering behind. That happened: an agent hung having injected a
  `lifecycleMu.Lock()` around a disk read, and only worktree isolation
  kept it out of the main checkout.

One thing the command's own body gets wrong if read literally: the
`smart` role has **no write tools** and always delegates to a worker.
Do not instruct such an agent not to spawn sub-agents — it will obey and
be unable to do the task. Constrain it to one worker at a time instead.

### The memory ceiling is shared, not per-process

5 GB is the operator's stated ceiling for what any one heavy operation
on this machine may claim — and it is a ceiling on top of everything
else already running (other agents, the IDE, the OS), not a personal
allowance for that operation alone. Claim well under it, not up to it.

Never run a full-tree `go test ./...`, `go build ./...` or
`golangci-lint run` bare. Wrap it in `win-nice`'s `capm`, and serialise
both across packages and within them: `capm 2g go test -p 1 -parallel 1
-count=1 ./...`. `-p 2`/`-parallel 2` is not "safely under the ceiling"
once every other process on the box is counted too.

Two failures from one session established why this is a hard rule, not
a suggestion: a bare `go test ./...` crashed the Go runtime itself with
a stack overflow (`Exception 0xc00000fd`, exit 139) — not a test
failure, the test binary dying — and three separate `errno=1455`
(`ERROR_COMMITMENT_LIMIT`) kills came from ordinary-looking parallel
Go builds/lints that individually looked fine. `capm 12g`, then `capm
5g`, were both tried and both corrected downward before `capm 2g -p 1`
finally ran clean twice in a row.

## What This Fork REMOVED — Do NOT Re-import

Upstream commits touching any of these MUST be skipped:

| Removed subsystem | Upstream-coded as | Skip rule |
|---|---|---|
| **Bubble Tea TUI** | `internal/tui/` | Any commit under `internal/tui/**` → SKIP |
| Upstream YOLO + permissions test files | `b46dae6c` removed them | SKIP fixes there — fork has no YOLO/auto-approve UI at all (removed `5c323b55`); only non-interactive `rush run` auto-approve remains |
| Taskfile.yaml, swagger stub, nix, playwright temp | `52bb90f8` | SKIP CI/build replays |
| CLA bot infra | `chore(legal): @… signed CLA` commits | SKIP — fork doesn't use CLA bot |
| Auto-update / scheduled chores | `chore: auto-update files`, cron `nightly.yml` | SKIP |
| `--effort high` thinking model variants (deprecated) | `8b3707dd` | SKIP attempts to re-add |
| localStorage settings (we use backend) | `8868dced` | SKIP UI commits that re-introduce client-side persistence |
| LSP integration | `internal/lsp/` (manager, client, all LSP-backed agent tools) | Fully removed, no `internal/lsp` package exists in the fork at all — confirmed no import of it anywhere. SKIP every upstream `fix(lsp)`/`feat(lsp)` commit, including ones that also touch shared files like `internal/agent/tools/*` (e.g. new `lsp_definition`/`lsp_rename`/`lsp_call_hierarchy`/`lsp_symbols` tools) — don't port the LSP-specific pieces even when they ride along in a commit that also changes non-LSP code. |

## What This Fork OWNS — Touch Upstream Only With Extreme Care

These subsystems are fork-rewritten. Upstream changes to the same area
are almost always wrong for us:

### Multi-client / multi-session
- **Upstream** is shipping a new multi-client server model (look for
  `feat(server): share one workspace per directory across clients`,
  `feat(db): refuse to open a data directory in use by another crush`,
  `feat(tui): auto-close permission prompt when another client
  responds`, etc.). **Skip all of it.**
- **Fork** uses lock-file + heartbeat model in `sessions_*.go`
  (10s touch, 20s expiry, auto-clear stale locks > 60s, `kill`
  enforces taskkill /F /T on Windows). This is intentional and
  battle-tested.
- **Fork's multi-session engine predates upstream's and is the more
  mature design for our use case** — it shipped before upstream even
  had a multi-client concept, was built for N truly concurrent
  `rush run` sessions from the start (see `coordinator.go`'s own
  "drives N concurrent web sessions" note), and doesn't share
  upstream's "one shared workspace across simultaneous UI clients"
  problem space at all. Do not treat upstream's session/concurrency
  fixes as a naturally superior reference just because they're newer.
- **This caution extends beyond files literally under
  `internal/server`/`internal/workspace`/`internal/client`.** Upstream
  occasionally fixes session-dispatch races in code we DO share
  (e.g. `internal/agent/coordinator.go`, `internal/agent/agent.go`) —
  even when such a commit isn't tagged `fix(server)`, treat it as
  **EVAL, not PORT**: first verify the same race is actually
  reachable through *our* session model (lock-file + heartbeat, N
  independent sessions) before assuming their fix — shaped for their
  shared-workspace-across-clients problem — applies to us at all, or
  applies in the same shape.

### Web UI / WebSocket
- **Upstream** has no web UI — they own the TUI.
- **Fork** replaced TUI entirely with React (`web/`) + WebSocket
  server. Any upstream `feat(ui)` / `fix(ui)` is for TUI and does
  not apply.
- Fork-side: `feat(web): …` commits. Upstream-side: `feat(tui): …`.
  Mapping is rarely 1:1.

### Agent / coordinator / CLI provider
- **Fork** has rewritten parts: `agentguard`, `cliprovider`, `hyper`,
  `agentic_fetch_tool`, `hooked_tool`, `loop_detection`,
  `stream_watchdog`, `usage_fallback`, `notify`, `prompt`.
- Upstream `fix(agent)` may apply if the fix targets shared code in
  `agent.go` / `coordinator.go`, but **always cherry-pick a clean
  patch** rather than accepting a merge that drags in adjacent files.
- Specifically: upstream's `fix(agent): centralize 401 retry logic`
  / `add 401 retry and reauth notification to sub-agent runs` /
  `fix(oauth): …` may be worth porting **if** the fork's oauth
  layer is identical. Check first.

### Provider-model registry
- **Fork** uses `cmd/models_atoms.go` (atoms + short codes:
  `fl`/`oh`/`s46xx`/...) and a curated `atomRegistry`.
- **Upstream** uses raw catwalk lists. Their `fix(cli): show all
  providers in crush models` already landed in spirit on the fork.
- New provider integrations from upstream (alibaba, bedrock europe,
  copilot models, qwen3.7-max fixes, …) → port the **client logic**
  only, leave atom registry to the fork.

### Slash-commands / agents installer
- **Fork**: `cmd/claude_init.go` + `claude_del.go` install `/rush`
  and per-model commands (`fl`, `ox`, etc.) plus sub-agents into
  `.claude/{commands,agents}/`.
- Upstream has nothing equivalent. Any "skills discovery" upstream
  change (`feat: discover skills from git root in monorepos`) is
  independent and may be safe to port.

### CI / hooks
- **Fork** has its own `nightly.yml` (cron disabled),
  `security.yml`, `.githooks/pre-push` mirroring CI.
- **Upstream** CI updates → review individually. Most are noise.
  Do NOT enable any cron on the fork (we explicitly disabled all
  scheduled workflows to stop emails).

## Merge Workflow

When the user asks to merge upstream:

1. **Fetch first**: `git fetch origin main` (origin = upstream
   charmbracelet, fork = our PHPCraftdream remote).
2. **Triage by category** before opening any merge:
   ```bash
   git log --oneline --no-merges origin/main ^main
   ```
   Bucket every commit into:
   - **PORT** — bug fix in shared code, no TUI/server/web touch
   - **EVAL** — provider/agent change that may apply, needs read
   - **SKIP** — TUI, multi-client server, CLA, auto-update, cron,
     anything in the "REMOVED" or "OWNED" tables above
3. **Cherry-pick the PORT bucket** one commit at a time. Do not run
   a bulk `git merge origin/main` — it drags in everything and
   creates conflicts in subsystems we own.
4. **For each ported commit**: rebuild, run `go test ./...`, run
   the pre-push hook (`.githooks/pre-push`). If anything breaks,
   bail on that commit and write a SKIP note.
5. **Document skips** in the merge commit message. Future-us needs
   to know why specific upstream commits never came over.

## Subagents Working on This Repo

If you are a delegated worker (Task tool / sub-agent), in addition to
the shared-workspace git-safety clause already in your prompt:

- **Do not bulk-merge upstream.** Ever. Even if asked. Surface the
  list, ask for confirmation per category.
- **Do not run project-wide test suites** (`go test ./...`,
  `make test`). Test only the packages you touched. The orchestrator
  runs the full suite at the end.
- **Do not touch CLAUDE.md** (this file) unless explicitly told to.
- **Do not touch `.github/workflows/`** to re-enable cron schedules.
- **Do not delete `web/dist/.gitkeep`** — it's required by
  `//go:embed all:dist` for CI builds. (It is currently deleted in
  the working tree; user knows; do not commit the deletion either
  way without explicit instruction.)
- **Never exercise `rush models`/`rush providers`/anything that
  writes config against the machine's real global config**, even
  when told to "isolate" via `--cwd`. `--cwd` only affects
  *workspace*-scope config; it does nothing for *global*-scope writes
  (`rush models use` defaults to global scope). The only thing that
  isolates global scope is setting the `RUSH_GLOBAL_DATA` env var to
  a throwaway directory for that invocation — confirmed necessary the
  hard way multiple times in one session, when "isolated" manual
  verification in an agent's own report turned out to have silently
  overwritten the operator's real, in-use model configuration anyway.
  If your task instructions say "isolate manual CLI exercising" and
  don't spell out `RUSH_GLOBAL_DATA` explicitly, set it yourself
  before running anything that writes — don't assume `--cwd` alone is
  enough, and don't trust your own past invocation was isolated
  without re-checking the real global config file afterward.
- **`RUSH_GLOBAL_DATA` isolates only ONE of two real config paths.**
  This machine (and likely any dev machine that's run `rush` outside
  a container) also has a SEPARATE real config file reached via
  `GlobalConfig()` (gated by `RUSH_GLOBAL_CONFIG`/`XDG_CONFIG_HOME`,
  not `RUSH_GLOBAL_DATA`) — found to hold the live provider API key
  and MCP server definitions, distinct from the `GlobalConfigData()`
  path the bullet above covers. Setting `RUSH_GLOBAL_DATA` alone
  still leaks real credentials/MCP config into a "manually isolated"
  run. For genuine isolation, set BOTH `RUSH_GLOBAL_DATA` and
  `RUSH_GLOBAL_CONFIG` to throwaway directories, or better, put test
  config in a project-local `rush.json` instead of relying on either
  global path — `GlobalConfigData()`'s target behaves as an
  app-managed scratch file the app itself can rewrite/clear between
  runs, not a stable place to seed test state.

## Quick Reference — Where Things Live

| Concern | Fork file |
|---|---|
| Atoms / short codes | `internal/cmd/models_atoms.go` |
| CLI providers (claude/gemini/codex/qwen) | `internal/agent/cliprovider/provider.go` |
| Ping (incl. CLI providers) | `internal/cmd/ping.go` |
| Sessions CLI family | `internal/cmd/sessions_*.go` |
| Slash-command / agent installer | `internal/cmd/claude_init.go`, `claude_del.go` |
| Slash-command body | `internal/cmd/claude_slash_command.md` |
| Agent guard rails | `internal/agent/agentguard/` |
| Stream watchdog | `internal/agent/stream_watchdog.go` |
| Loop detection | `internal/agent/loop_detection.go` |
| Web UI | `web/` |
| Pre-push hook | `.githooks/pre-push` |
