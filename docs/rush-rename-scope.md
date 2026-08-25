# crush → rush rename — scope map

Task #699. Produced by grep-based inventory of the repo at commit `0442f318`. Read this before starting any of tasks #700-#711.

## Confirmed decisions (user, 2026-08-24)

- Go module import path **changes**: `github.com/charmbracelet/crush` → `github.com/PHPCraftdream/rush`, rewritten in every internal import.
- New GitHub repo: `PHPCraftdream/rush`.
- New npm package: same scope, new name. Current package is `@phpcraftdream/crush` (see below) → new package is `@phpcraftdream/rush`.

## Buckets

### 1. Go import paths — task #700
`github.com/charmbracelet/crush` appears in **23,459 lines** across every `.go` file in the module (every internal import statement, effectively the whole tree). This is the single largest mechanical change — script it (goimports-aware rewrite or a scoped sed across `*.go`), don't hand-edit.

### 2. go.mod module line — task #700
```
module github.com/charmbracelet/crush
```
→ `module github.com/PHPCraftdream/rush`

### 3. Go identifiers (capitalized `Crush`, non-import-path) — task #701
**54 `.go` files** under `internal/` and `cmd/` contain a case-sensitive `Crush` identifier (type/var/func names, string literals, comments) distinct from the lowercase import-path string. Needs semantic review file-by-file — some of these may be legitimate upstream-attribution comments ("this differs from upstream charmbracelet/Crush's...") that should stay as-is.

### 4. Environment variables — task #702
Every `CRUSH_`-prefixed env var found (47 distinct names, values from `grep -rohE "CRUSH_[A-Z_]+"`):

```
CRUSH_CACHE_DIR
CRUSH_CLIPROVIDER_LOG_RAW_PROMPT
CRUSH_CODEX_MCP_TOKEN
CRUSH_COLOR_SCHEME
CRUSH_CORE_UTILS
CRUSH_COST_USD
CRUSH_CWD
CRUSH_DEPLOY_PATH
CRUSH_DISABLE_ANTHROPIC_CACHE
CRUSH_DISABLE_DEFAULT_PROVIDERS
CRUSH_DISABLE_PROVIDER_AUTO_UPDATE
CRUSH_DURATION_SEC
CRUSH_ENV_OVERLAY_TEST_PROBE
CRUSH_ENV_OVERLAY_UNSET_BEFORE_PROBE
CRUSH_EVENT
CRUSH_EXIT_REASON
CRUSH_FOO                              (likely a test fixture literal, verify before renaming)
CRUSH_FORBID_WRITES
CRUSH_GLOBAL_CONFIG
CRUSH_GLOBAL_DATA
CRUSH_HYPER_API_KEY
CRUSH_LOCK_HELPER_DATADIR
CRUSH_LOCK_HELPER_HOLD_SECONDS
CRUSH_LOCK_HELPER_PROCESS
CRUSH_LOCK_HELPER_SESSIONID
CRUSH_LOG_HTTP_BODIES
CRUSH_MAX_BACKGROUND_JOBS
CRUSH_MCP_TEST_HELPER
CRUSH_OPENAI_API_KEY
CRUSH_PROFILE
CRUSH_PROJECT_DIR
CRUSH_PROVIDER_CACHE_ONLY
CRUSH_PROVIDER_CACHE_TTL
CRUSH_RUN_DEFAULT_HARD_TIMEOUT
CRUSH_SESSION_ID
CRUSH_SESSIONS_CRASH_LOCK_HELPER
CRUSH_SESSIONS_CRASH_LOCK_HELPER_DATADIR
CRUSH_SESSIONS_CRASH_LOCK_HELPER_SESSIONID
CRUSH_SESSIONS_KILL_LOCK_HELPER
CRUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR
CRUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID
CRUSH_SESSIONS_SWEEP_NEWOWNER_DATADIR
CRUSH_SESSIONS_SWEEP_NEWOWNER_HELPER
CRUSH_SESSIONS_SWEEP_NEWOWNER_SESSIONID
CRUSH_SHELL_TEST_HELPER
CRUSH_SKILLS_DIR
CRUSH_TEST_FAST_EXIT_FILE
CRUSH_TOKENS
CRUSH_TOOL_INPUT_COMMAND
CRUSH_TOOL_INPUT_FILE_PATH
CRUSH_TOOL_NAME
CRUSH_X                                (likely a test fixture literal, verify before renaming)
```
Rename every one to `RUSH_`-prefixed, except confirm `CRUSH_FOO`/`CRUSH_X` aren't intentionally-generic test placeholders that don't need to track the real prefix (check their call sites — if they're literally testing "does this code correctly read ANY env var", the exact prefix doesn't matter and renaming is still fine for consistency, just noting they're lower-stakes).

`CRUSH_GLOBAL_CONFIG` / `CRUSH_GLOBAL_DATA` are the two most operationally significant (per CLAUDE.md's own documented isolation guidance for this fork) — their rename must be reflected in CLAUDE.md too (task #705).

### 5. npm package — task #706
Current: `npm/crush/package.json` → `"name": "@phpcraftdream/crush"`.
New: `@phpcraftdream/rush` (same scope, per user decision), likely living at `npm/rush/`.

### 6. CI workflow files mentioning "crush" — task #703
```
.github/workflows/build.yml
.github/workflows/labeler.yml
.github/workflows/lint-sync.yml
.github/workflows/nightly.yml
.github/workflows/publish-fork-npm.yml
.github/workflows/release.yml
.github/workflows/schema-update.yml
.github/workflows/snapshot.yml
.githooks/pre-push
```
Note: `lint.yml` and `security.yml` did NOT match this grep — they likely only reference `.golangci.yml`/generic config, not the literal word "crush". Verify during #703 rather than assuming they're untouched (they may still have job/step names worth checking).

### 7. web/ occurrences — task #704
**17 files** under `web/src`, `web/package.json`, and web config/HTML mention "crush" case-insensitively. Check page title, header/status-bar UI text, and `package.json`'s `name` field specifically.

### 8. Docs — task #705
**6 files**: `README.md`, `CLAUDE.md`, `CHANGELOG*`, and files under `docs/*.md`. README needs the single deliberate "fork of crush" exception; everything else in this bucket becomes fully "rush".

## Not yet counted (flag for #708's final sweep)

- `.claude/commands/crush.md`, `.claude/agents/*` installer output, and the `/crush` skill body itself — covered by task #702's explicit installer-rename scope, but re-verify in #708's final grep since these are generated/copied files, not hand-maintained source.
- `docs/reviews/*.md` and `docs/checkpoints/*.md` — this session's own review/checkpoint artifacts almost certainly mention "crush" extensively (they're about THIS fork). Decide during #705/#708 whether historical review docs get renamed too (probably NOT worth rewriting historical artifacts — they're a record of what was true at the time) or left as-is with a one-line note that they predate the rename. Lean toward leaving them untouched; flag this explicitly rather than silently deciding either way.
- go.sum / vendor directories (if any) — do not touch, these are third-party dependency checksums/code, irrelevant to this project's own naming.

## Verification anchor

Task #708's final check: `grep -ri crush` across the whole repo (excluding `.git/`, `node_modules/`, `.claude/worktrees/`, and the historical `docs/reviews/`+`docs/checkpoints/` artifacts per the note above) should return exactly one hit — the deliberate README attribution line from task #705.
