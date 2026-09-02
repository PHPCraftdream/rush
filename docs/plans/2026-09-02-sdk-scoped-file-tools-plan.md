# SDK scoped file tools: per-folder ACL + batch actions — design plan

Date: 2026-09-02. Status: planning only, no code written. Read-only
investigation of the tree at `49612bfb` (plus the uncommitted working
tree); every claim below cites `file:line` so it can be re-checked rather
than believed.

Scope of this document: an SDK host (`*sdk.Client`) wants to hand ONE
agent turn a scoped, batch-capable filesystem toolset instead of today's
all-or-nothing coder toolset. Two requirements from the operator:

1. **Per-folder tool scoping** — the call passes an array of folder
   scopes; each names a directory and the file operations allowed inside
   it (list, find-by-name, grep, read, read-by-range, read-by-radius
   around grep hits, write-by-range, full write, create, delete, replace).
2. **Batch/array actions** — every operation accepts an ARRAY of targets
   in one tool call, with a defined partial-failure policy.

Plus two questions flagged in the design conversation: what to do about
`bash` (the escape hatch), and where to enforce (coarse toolset filter vs
fine per-path check).

---

## 1. Current-state summary

All file tools live in `internal/agent/tools/`. Every one is a
`fantasy.NewAgentTool` closure over `workingDir` taking ONE target;
relative paths go through `filepathext.SmartJoin(workingDir, p)`
(`internal/filepathext/filepath.go:11-16`), which passes an absolute path
through UNCHANGED — so any tool that does not explicitly check
containment accepts any absolute path on the machine.

| Operator vocabulary | Existing tool | Params today | Path check today | Array of targets? |
|---|---|---|---|---|
| list files | `ls` | `LSParams{Path, Ignore []string, Depth}` (`ls.go:20-24`) | outside-workdir → `permissions.Request(action "list")` (`ls.go:97-123`); inside → none | No (one `Path`) |
| search by name/pattern | `glob` | `GlobParams{Pattern, Path}` (`glob.go:41-44`) | **None** — no permission request, no containment check; `Path` may be any absolute dir (`glob.go:60`) | No |
| search by content | `grep` | `GrepParams{Pattern, Path, Include, LiteralText}` (`grep.go:71-76`) | **None** (`grep.go:183`) | No |
| read whole file | `view` | `ViewParams{FilePath, Offset (0-based), Limit (default 500)}` (`view.go:46-50`) | outside-workdir → `permissions.Request(action "read")` (`view.go:143-179`); skills paths exempt via `isInSkillsPath` (`view.go:442-475`, the only symlink-aware containment check in the tree) | No |
| read by line range | `view` | same, `Offset`+`Limit` | same | No |
| read by radius around grep hits | **none** | — | — | — |
| write by line range | **none** | — | — | — |
| full write (overwrite) | `write` | `WriteParams{FilePath, Content}` (`write.go:30-33`) | `CheckForbiddenWrite` env blacklist (`forbidden_writes.go:25-44`), then `permissions.Request(action "write")` (`write.go:125-140`); no containment check | No |
| create | `write` (creates + `MkdirAll` parent, `write.go:101-109`) or `edit` with empty `old_string` (`edit.go:86-87`, `createNewFile` refuses an existing file `edit.go:107-113`) | as above | as above | No |
| delete (file) | **none** — the only `os.Remove` in the package is `download.go:187`'s temp cleanup. Deleting a file today is only possible via `bash rm`. | — | — | — |
| replace content (find/replace) | `edit` (`edit.go:28-33`, unique-match or `replace_all`, `findAndReplace` `edit.go:213-232`) and `multiedit` (`multiedit.go:27-36`, N edits on ONE file) | `EditParams{FilePath, OldString, NewString, ReplaceAll}` / `MultiEditParams{FilePath, Edits []MultiEditOperation}` | `CheckForbiddenWrite` + `permissions.Request("write")`; **requires a prior read** in this session via `filetracker.LastReadTime` (`edit.go:307-310`) | `multiedit`: array of EDITS, still one FILE |

Distance from the two requirements, per tool:

- **Working-dir-relative path it could check against a scope**: every tool
  already computes an absolute path (`SmartJoin` + `filepath.Abs`), so
  the *input* is one `filepath.Rel` away from a scope check. What is
  missing is (1) any containment check at all in `glob`/`grep`/`write`/
  `edit`/`multiedit`, (2) symlink resolution anywhere except
  `isInSkillsPath`, and (3) post-filtering of *results*: `glob`/`grep`/
  `ls` return paths, and a search rooted inside a scope can still surface
  paths under a denied sub-folder.
- **Array of targets**: none of the seven tools takes an array of paths.
  `multiedit` is the only precedent for "N operations, per-item outcome
  in one response" (`FailedEdit` list in `MultiEditResponseMetadata`,
  `multiedit.go:44-57`, best-effort application in `applyEditsToContent`
  `multiedit.go:133-148`, whole-call error only when every edit failed
  `multiedit.go:285-297`).

Other relevant precedents read for this plan:

- `agentguard` (`internal/agent/agentguard/agentguard.go`) is a bash
  COMMAND-STRING denylist (AI-agent recursion + Windows window-openers),
  not a path ACL — its reusable lesson is structural: one typed
  `*DeniedError` (`:27-39`) and ONE `CheckAll` entry point (`:171-190`)
  deliberately shared by both bash surfaces (built-in `bash.go:231` and
  the cliprovider MCP `Bash` `mcpserver_tools.go:136`) "so a tool surface
  cannot wire only a subset of the guards". The folder-scope matcher must
  follow the same rule — one `Check`, every surface — which is exactly
  what section 3 finds is NOT yet possible for the cliprovider surface.
- `RunAllowlistSpec` → `BuildRunAllowlist` → `RunAllowlist`
  (`internal/permission/runallowlist.go:35-53`, `:148-176`, `:74-126`) is
  the "user-facing spec, compiled once, queried many times, total matcher,
  deny is the safe direction" template. Its matcher is keyed by tool name
  / `tool:action` and, for command tools, by the command string
  (`:108-126`); it is path-blind.
- `permission.Request` (`internal/permission/permission.go:392-541`): the
  non-interactive auto-approve branch consults per-call entry → session
  baseline → process-wide gate (`:443-461`) and on a restricted miss
  returns `false`, which every tool turns into
  `NewPermissionDeniedResponse()` — a **turn-ending** response
  (`StopTurn = true`, `tools.go:138-142`). That is the right severity for
  "the human/operator said no", and the wrong severity for "item 7 of 20
  is outside its folder" (section 5).
- R3-1 per-call tool pinning: `CallOptions` (`call_options.go:34-89`) →
  `buildTools` reads it from ctx (`coordinator_tools.go:327-329`) →
  `pinCallTools` (`coordinator_models.go:464-486`) → `resolvedOverrides.tools`
  → `pin` onto `SessionAgentCall.Tools` (`coordinator_models.go:405-407`,
  `agent.go:284-296`) → consumed at turn start and every `PrepareStep`
  (`agent_turn.go:284-287`, `:1080-1084`); `UpdateModels` strips
  `CallOptions` so a per-call filter can never be published
  (`coordinator_models.go:493-495`, `:727`). Sub-agents built for the call
  (`buildTools` → `agentTool(ctx)` → `buildAgent(ctx, …, true)` →
  `buildTools(ctx, …)`, `agent_tool.go:44`, `coordinator_tools.go:143-150`)
  are built with the SAME ctx, so a per-call filter automatically shapes
  the sub-agent's toolset too.
- `fantasy.ToolResponse` (`charm.land/fantasy@v0.25.2/tool.go:32-42`) is
  `{Type, Content string, Data []byte, MediaType, Metadata string, IsError,
  StopTurn}`; `executeSingleTool` (`fantasy/agent.go:743-780`) maps it to
  exactly one of `ToolResultOutputContentText | …Error | …Media`. There is
  no multi-part or structured result in the protocol: a batch response
  must be encoded inside `Content` (model-facing text) and `Metadata`
  (JSON for UIs/hosts).

---

## 2. Gap analysis

| Gap | Why no existing tool covers it | Minimal capability that closes it |
|---|---|---|
| **Read by radius around grep hits** (the operator's flagged case) | `grep` emits ONE trimmed line per match, capped at 500 chars, no context (`grep.go:208-217`); ripgrep is invoked with `--json -H -n -0` and only `"match"` messages are parsed (`rg.go:52`, `grep.go:309`). Getting N lines around each hit today costs 1 + (number of hits) tool calls (`grep` then a `view` with `offset`/`limit` per hit). **Confirmed: not mappable onto any existing tool in one call.** | `fs_grep` gains `context_lines` (radius, 0–50). ripgrep path: add `-C N`; `rg --json` then emits `"type":"context"` messages with the same `data.path`/`data.lines.text`/`data.line_number` shape as `"match"`, so the existing parser (`grep.go:380-394`) needs one extra `Type` case. Fallback regex path (`fileMatches`, `grep.go:507-581`): keep a ring buffer of the last N lines and read ahead N lines after a hit. Output: one numbered block per hit `[line−N, line+N]`, hit lines marked, adjacent/overlapping blocks per file merged, total bytes capped. Secondary, cheap form: `fs_read` items accept `{path, line, radius}` (centre ± radius) as an alternative to `{path, start_line, end_line}`. |
| **Write by line range** | `edit`/`multiedit` are string-anchored (exact `old_string`), never line-anchored; `write` is whole-file only. | `fs_write_lines` items `{path, start_line, end_line, content}` (1-based, inclusive; `end_line = start_line − 1` inserts before `start_line`; empty `content` deletes the range). Same guards as `edit`: read-before-write via `filetracker`, CRLF preservation (`fsext.ToUnixLineEndings`/`ToWindowsLineEndings` as `edit.go:328`, `:382-385`), atomic write, history versions. Items on the same file are grouped, overlapping ranges rejected per item, non-overlapping ranges applied bottom-up (descending `start_line`) so earlier ranges never shift later ones, one atomic write per file. |
| **Delete** | No tool; only `bash rm`. | `fs_delete` items `{path}`; files only in v1 (no directories, no recursion), resolved through the same scope resolver so a symlink inside a scope pointing outside is denied. |
| **Batch (arrays) for every op** | Every existing tool is single-target (`multiedit` is single-file). | Every `fs_*` tool takes `items []`. Retrofitting arrays onto the seven legacy tools is deliberately NOT proposed (section 3 explains why the scoped call gets a new family instead). |
| **Result post-filtering for searches/listings** | `glob`/`grep`/`ls` never filter results by policy. | `fs_find`/`fs_grep`/`fs_list` check every returned path with the same matcher as reads, and drop paths under a deny carve-out (section 3). |
| **Symlink-aware containment** | Only `isInSkillsPath` resolves symlinks; the write tools' `MkdirAll` and `AtomicWriteFile` follow whatever the raw path points at. | One resolver (`resolveScopedPath`) used by every `fs_*` item: `SmartJoin` → `Abs` → `Clean` → `EvalSymlinks` of the longest EXISTING prefix (so a not-yet-created file is judged by its resolved parent). |

Not a gap, but a finding that changes the enforcement design (section 3):
**the cliprovider path does not use the coordinator's tool slice at all.**
When the smart model is a CLI provider (`claude`/`codex`/`gemini`/`qwen`,
`cliprovider.ProviderType = "cli"`, `provider.go:55`), the sub-process gets
`--allowedTools mcp__rush__Bash,Read,Write,Glob,Grep,todos,WebSearch,WebFetch,Task,Agent`
(`provider_stream.go:158-188`) and those `mcp__rush__*` tools are SEPARATE
implementations in `mcpserver_tools.go` (`Read` `:179-236`, `Write`
`:263-304`) whose `resolvePath` accepts any absolute path (`:541-556`) and
whose permission requests use the fixed session id `"cli-mcp"` (`:526`).
Nothing pinned on `SessionAgentCall.Tools` reaches that process.

---

## 3. Enforcement architecture recommendation

**Answer to (a)/(b): both, with a clean split — and the scoped call gets a
NEW `fs_*` tool family rather than retrofitted legacy tools.**

### 3.1 The split

- **(a) Coarse — which tools exist for this call** — rides the R3-1
  `CallOptions` → `buildTools` → `pinCallTools` pipeline unchanged. A
  scoped call's toolset is computed once, pinned on the call, never
  published. Used for everything that must be *unforgeable*: removing the
  escape hatches (section 4), removing the legacy single-target file
  tools, and including only those `fs_*` tools whose operation is granted
  by at least one scope entry (e.g. no `fs_delete` unless some entry
  grants `delete`). A tool that is absent cannot be called, no matter what
  `permissions.allowed_tools`, YOLO (`SetSkipRequests`) or a PreToolUse
  hook approval says — all three are Request-time bypasses
  (`permission.go:393-413`) and cannot reintroduce a tool.
- **(b) Fine — which paths/ranges/operations each item may touch** —
  enforced inside each `fs_*` tool's `Run()`, per item, through ONE
  compiled matcher (`permission.FolderScope.Check(absResolvedPath, op)`)
  plus post-filtering of search/list results. Denials are level-1 per-item
  results (section 5), never `permissions.Request` denials.

Which vocabulary entries need which layer:

| Op | (a) coarse | (b) fine |
|---|---|---|
| list | `fs_list` present iff some entry grants `list` | each root ∈ a `list` scope; listed entries under a deny carve-out dropped |
| find | `fs_find` iff `find` | root check + every result path checked with `find` |
| grep (+ radius) | `fs_grep` iff `grep` | root check + every match path checked with `grep`; radius never changes the file, so no extra check |
| read / range / radius | `fs_read` iff `read` | per item path check; range/radius shape validated |
| create, overwrite | `fs_write` iff `create` OR `overwrite` | per item: existence decides which op is checked (`create_only` flag forces `create`); resolved PARENT must be in scope (covers `MkdirAll`) |
| write by range | `fs_write_lines` iff `write_lines` | per item path check; read-before-write; overlap rejection |
| replace | `fs_replace` iff `replace` | per item path check; read-before-write |
| delete | `fs_delete` iff `delete` | per item; resolved target must be in scope (symlink escape denied) |

Why a new family instead of scope-checking the seven legacy tools in
place or via a wrapper:

1. The legacy tools cannot satisfy requirement 2 (arrays) without
   changing their `Params` shape, which the CLI/web paths and the
   cliprovider MCP mirror depend on. New tools change nothing for
   unscoped callers.
2. A generic wrapper (the `hookedTool` shape, `hooked_tool.go:54-100`)
   could pre-check `file_path`/`path` from `call.Input`, but it cannot
   post-filter `glob`/`grep`/`ls` results without parsing rendered text,
   and it would have to know each tool's param spelling. Putting the
   check inside tools that are new code anyway is simpler and total.
3. The heavy lifting is already package-level and reusable:
   `readTextFile` (`view.go:343-398`), `addLineNumbers` (`:318-341`),
   `findAndReplace` (`edit.go:213-232`), `loadExistingFile`
   (`edit.go:284-330`), `commitFileChange` (`edit.go:246-275`),
   `searchFiles` (`grep.go:239-264`), `globFiles` (`glob.go:89-101`),
   `ListDirectoryTree` (`ls.go:138-169`), `CheckForbiddenWrite`,
   `fsext.AtomicWriteFile`. The new tools are mostly batching + scope
   around these.
4. It gives the scoped call ONE consistent convention (1-based inclusive
   line numbers everywhere, `items []` everywhere) instead of `view`'s
   0-based `offset` next to a 1-based range tool.

A host that wants batch tools without restriction passes one entry
`{Dir: ".", Ops: <all>}` — the family is selected by "scopes present",
not by "scopes narrow".

### 3.2 Where the matcher lives and its shape

New file `internal/permission/folderscope.go` (a sibling of
`runallowlist.go`, same package, **no edits to `permission.go`** — the
matcher deliberately does not hook `Request`, see 3.3). Reasons for the
same package rather than a new one: identical spec→compile→query
vocabulary and doc conventions ("total matcher", "deny is the safe
direction"); `tools` and `agent` already import `permission` so no new
import edge; a new file cannot conflict with the concurrent
`permission.go` edits of the r4-1-2-3 worker.

```go
// FileOp is one file operation a folder scope may grant.
type FileOp string

const (
	FileOpList       FileOp = "list"        // fs_list
	FileOpFind       FileOp = "find"        // fs_find
	FileOpGrep       FileOp = "grep"        // fs_grep (incl. context radius)
	FileOpRead       FileOp = "read"        // fs_read (whole / range / radius)
	FileOpCreate     FileOp = "create"      // fs_write on a non-existent path
	FileOpOverwrite  FileOp = "overwrite"   // fs_write on an existing file
	FileOpWriteLines FileOp = "write_lines" // fs_write_lines
	FileOpReplace    FileOp = "replace"     // fs_replace
	FileOpDelete     FileOp = "delete"      // fs_delete
)

// FolderScopeEntry is one host-facing scope: a directory subtree and the
// operations granted inside it. Dir is absolute or WorkingDir-relative
// ("." = the working directory). Ops empty = deny carve-out: the subtree
// is excluded from every enclosing entry's grants (and from listings and
// search results).
type FolderScopeEntry struct {
	Dir string
	Ops []FileOp
}

// FolderScopeSpec is the user-facing, pre-compilation form, mirroring
// RunAllowlistSpec. BuildFolderScope compiles it into a FolderScope.
type FolderScopeSpec struct {
	// WorkingDir resolves relative Dir entries. Set by ExecuteRun from the
	// App's working directory; never by the host.
	WorkingDir string
	Entries    []FolderScopeEntry
	// KeepCommandTools keeps bash/run_command/job_* in the scoped toolset.
	// ExecuteRun sets it ONLY when the same call is RestrictedRun with at
	// least one AllowBash pattern (section 4); hosts never set it directly.
	KeepCommandTools bool
}

// FolderScope is the compiled, immutable, concurrency-safe matcher. The
// zero value grants nothing (deny-all); a nil *FolderScope on CallOptions
// means "unscoped call" (legacy toolset), which is a different thing.
type FolderScope struct {
	entries []compiledScopeEntry // absolute, cleaned, symlink-resolved Dir; sorted longest Dir first
	granted map[FileOp]struct{}  // union of all Ops, for the coarse (a) decision
	keepCommandTools bool
}

func BuildFolderScope(spec FolderScopeSpec) (FolderScope, error) // ANY bad entry is an error (section 6)
func (s FolderScope) Grants(op FileOp) bool                       // coarse: some entry grants op
func (s FolderScope) Check(absResolvedPath string, op FileOp) error // fine: nil = allowed; *ScopeDeniedError otherwise
func (s FolderScope) Roots(op FileOp) []string                      // dirs granting op — default search/list roots
func (s FolderScope) KeepsCommandTools() bool

// ScopeDeniedError is typed (agentguard.DeniedError precedent) so batch
// runners can render it per item and tests can errors.As it.
type ScopeDeniedError struct{ Path string; Op FileOp; Reason string }
```

Matching rules (pure string logic, total, no I/O — the resolver that does
I/O lives in `tools`, see below):

- Longest-matching `Dir` wins; a path matches exactly one entry. An entry
  with empty `Ops` under a permissive parent is therefore a deny
  carve-out (`src/` read+grep, `src/secrets/` nothing). Union-of-ops
  across overlapping entries was rejected because it cannot express
  exclusion, which is the one overlap a host realistically needs.
- Containment = `filepath.Rel(dir, path)` succeeds and does not start
  with `..` — the same predicate as `isInSkillsPath` (`view.go:468-469`)
  and `fsext.HasPrefix` (`fileutil.go:219-226`). On Windows `Rel` compares
  case-insensitively, matching `CheckForbiddenWrite`'s lowercase
  normalisation (`forbidden_writes.go:46-56`). Cross-volume `Rel` errors
  deny.
- `Check` on a path that matches no entry denies ("outside every scope").
- `Grants(op)` ignores carve-outs (they grant nothing).

Path resolution (`internal/agent/tools/fs_scope.go`,
`resolveScopedPath(workingDir, raw string) (abs string, err error)`):
`SmartJoin` → `filepath.Abs` → `Clean` → `EvalSymlinks` of the longest
existing prefix, re-joining the non-existent tail (so `create` is judged
by the resolved parent, and `fs_write` cannot be pointed at a symlinked
directory that escapes). Any resolution failure → item denied (deny is
the safe direction; the legacy `isInSkillsPath` already returns false on
`EvalSymlinks` error, `view.go:452-455`). Scope `Dir`s are resolved the
same way at compile time (an entry whose `Dir` does not exist is still
accepted: the host may scope a folder the agent is expected to create —
but it is resolved against its longest existing prefix).

### 3.3 Why the matcher is NOT wired through `permissions.Request`

- `Request`'s restricted-run deny is rendered as `NewPermissionDeniedResponse()`
  with `StopTurn = true` (`tools.go:136-142`), because a human/operator
  "no" should end the turn. A per-item scope miss is model-correctable
  input (pick another path) — level 1 in the error contract
  (`tools.go:64-134`), and inside a batch it must not even fail the other
  items.
- `RunAllowlist` is keyed by tool/action/command (`runallowlist.go:108-126`);
  adding a path dimension would entangle two policies with different
  lifetimes (arming per turn via `SetSessionRunAllowlistForCall`,
  `permission.go:644-661`) for no gain — the scope is enforced by the
  tools that were BUILT for the call, so it needs no per-session arming,
  no inheritance to sub-agent session ids
  (`coordinator_subagents.go:107-126` stays untouched), and no clear-at-end.
- The write-side `fs_*` tools still call `permissions.Request` ONCE per
  call (as `multiedit` does for N edits, `multiedit.go:309-321`) with
  action `write`/`delete` and a description naming the item count — that
  keeps the interactive prompt path, the restricted-run gate and the
  notification/audit stream intact. Read/search `fs_*` tools never call
  `Request`: inside a scope the scope IS the authorisation, and outside
  it the item is denied before `Request` could be reached (legacy `view`
  only prompted for outside-workdir reads, `view.go:160`).

### 3.4 How it rides the R3-1 pipeline

1. `CallOptions` gains `FolderScope *permission.FolderScope` (compiled,
   immutable — consistent with the "immutable after construction"
   contract, `call_options.go:19-21`). nil = unscoped call.
2. `buildTools` (`coordinator_tools.go:327-329`) gets a third per-call
   filter, applied AFTER `buildToolsAgentConfigForCall` — ordering
   matters because worker layering ADDS `bash`/`edit`/`write`
   (`workerToolNames`, `coordinator_tools.go:171`) to a sub-agent's
   `AllowedTools`, and the scope filter must see the final list:

   ```go
   agent = c.applyCallDisableSubAgents(ctx, cfg, agent, isSubAgent)
   agent = c.buildToolsAgentConfigForCall(ctx, cfg, agent, isSubAgent)
   agent = c.applyCallFolderScope(ctx, agent) // new; no-op when CallOptions.FolderScope == nil
   ```

   `applyCallFolderScope` removes from `AllowedTools`: the legacy file
   tools (`view glob grep ls write edit multiedit`), the escape hatches
   (`bash run_command job_output job_kill download git_read agentic_fetch
   list_mcp_resources read_mcp_resource`; `bash run_command job_output
   job_kill` are kept when `KeepsCommandTools()`), and every `fs_*` whose
   op is not `Grants`-ed; and sets `AllowedMCP = map[string][]string{}`
   (the existing "no MCPs" spelling, `config.go:870`, honoured at
   `coordinator_tools.go:455-459`).
3. The `fs_*` tool names are added to `config.allToolNames()`
   (`config.go:787-814`) and the four read-side ones to
   `resolveReadOnlyTools` (`config.go:832`), so the existing
   `AllowedTools` filter (`coordinator_tools.go:437-442`),
   `Options.DisabledTools`, and the Task sub-agent's read-only default all
   keep working without a second mechanism. (Consequence worth stating: a
   plain `agent` sub-agent inside a scoped call gets `fs_list fs_find
   fs_grep fs_read`; a worker sub-agent gets whatever the scope grants,
   minus command tools unless `KeepsCommandTools`.)
4. `buildTools`' construction block (`coordinator_tools.go:402-424`) adds
   the family only when scoped:
   `tools.NewFSTools(tools.FSDeps{Permissions, History, Filetracker,
   WorkingDir, Grep: cfg.Tools.Grep, Ls: cfg.Tools.Ls}, scope)` returning
   the tools for granted ops; the `AllowedTools` filter then applies as
   for every other tool. The scope value is captured by the closures at
   build time, so `Run()` never reads ctx for policy — same discipline as
   the pinned toolset itself.
5. Nothing else in the pinning pipeline changes: `pinCallTools`,
   `resolvedOverrides.pin`, `runTurn`'s consumption and `UpdateModels`'
   `withoutCallOptions` all treat the slice opaquely. The existing tests
   in `coordinator_tool_pinning_test.go` are the template for the new
   ones (section 8, T8).

### 3.5 Surfaces the pipeline does NOT reach — fail closed

- **cliprovider** (section 2's finding): a scoped call whose resolved
  smart (or worker) provider has `Type == cliprovider.ProviderType` must be
  REFUSED with an error before any provider traffic, because the CLI
  sub-process would run `mcp__rush__Read/Write/Bash` implementations that
  know nothing of the scope. Location: the three per-call resolvers that
  already know the resolved provider config
  (`resolveSessionModelsInternal` `coordinator_models.go:200-213`,
  `applyModelOverrides` `:364-367`, `resolveCredentialsModels` in
  `credentials.go`), via one shared helper that reads
  `callOptionsFrom(ctx).FolderScope`. Threading the scope into
  `newRushMCPServer` (`mcpserver.go:88`) and the five MCP tools is a
  follow-up, not v1 (section 7). `CredentialSet` provider types cannot
  name a CLI provider, so `RunWithCredentials` only hits this through
  `AllowConfiguredRoleFallback`.
- **agentic_fetch**: builds its own `view`/`glob`/`grep` rooted in a temp
  dir (`agentic_fetch_tool.go:171-178`) under an unconditionally
  auto-approved child session, so an absolute path handed to that inner
  `view` reads anything. Stripped for scoped calls (it is already stripped
  by `DisableSubAgents` for every `rush run`/SDK default).
- **External MCP tools**: a filesystem MCP server would bypass the scope
  wholesale; `AllowedMCP = {}` for scoped calls (3.4 step 2). Library-mode
  clients start no MCP servers anyway (`sdk.go:19-32`).
- **Durable-queue restart**: a pump-rebuilt call carries no `CallOptions`
  and no `Tools` (`agent.go:293-296`, `:343-347`), so a scoped turn that
  is orphaned into the durable queue would restart with the UNSCOPED
  shared toolset — the exact fail-open shape of finding F2
  (`docs/reviews/2026-09-01-sdk-review-fh.md:92`). The r4-1-2-3 worker is
  building the restart-policy persistence for the `RunAllowlist` axis
  right now; the folder scope needs the analogous treatment (the SPEC is
  plain JSON-serialisable data, so `SessionAgentCallData` can carry it and
  the pump can recompile it). This plan does not propose that mechanism;
  it records the dependency (section 7, 8/T12).

---

## 4. Bash interaction recommendation

**A folder-scoped call has no `bash`, `run_command`, `job_output` or
`job_kill` tool — unless the SAME call is `RestrictedRun` with at least one
`AllowBash` pattern, in which case those four stay and are governed by the
existing R3-4 per-call `RunAllowlist` (deny-by-default, compound-command
guarded).** No new bash mechanism, no third state.

Reasoning, from what already exists:

- The existing restricted-run gate is precisely "bash restricted to a
  matching pattern set": `allowsRequest` routes every command-carrying
  tool (bash and run_command, via `RunAllowlistCommand()`,
  `bash.go:40-42`, `run_command.go:62-75`) to `bashCommandAllowed`
  (`runallowlist.go:116-124`, `:279-319`), ignores `AllowTools` for them,
  refuses compound commands for prefix/exact/glob forms, and is armed per
  call and per turn (`agent_run.go:492-493`). Re-implementing any of that
  for scoping would be a second, weaker copy.
- Removing the tool beats denying at `Request` time: a `RunAllowlist`
  denial ends the turn (`StopTurn`), so a scoped call with `RestrictedRun`
  but ZERO bash patterns would let the model call `bash`, get denied, and
  lose the turn. Absence costs nothing. Hence the "≥ 1 pattern" condition.
- Tool absence is also stronger than any permission-layer decision:
  `permissions.allowed_tools` containing `bash` is a global bypass checked
  BEFORE the gate (`permission.go:397-401`, and the `run.go:253-257` help
  text warns about exactly this), YOLO short-circuits everything
  (`:393-395`), and a PreToolUse hook `allow` pre-approves the prompt
  (`:407-413`). None of them can conjure a tool that was never built for
  the call.
- The residual risk under `KeepCommandTools` is the operator's explicit
  choice and must be documented in the same "accepted KNOWINGLY" voice as
  `orchestratorStrippedToolNames` (`coordinator_tools.go:181-186`): an
  allow-listed `go test ./...` or `npm run build` reads and writes files
  the scope never mentioned. The plan does not try to make that airtight
  (it would require an OS sandbox, which `config.RunPermissions`'s own doc
  already names as the only real alternative, `config.go:310-311`).
- Not chosen: "scoping implies bash off, period" — it would force hosts
  that need `go test` to give up scoping entirely; "orthogonal, operator
  decides freely" — it lets a host pass scopes AND an unrestricted bash
  and believe the scopes mean something. The rule above is the smallest
  one under which scopes are never silently meaningless.

Interaction rule that MUST be implemented alongside (a footgun found in
the code path, not a design nicety): under `RestrictedRun`, every
non-command tool is denied unless listed in `AllowTools`
(`runallowlist.go:133-146`, "an empty table denies every plain tool").
`ExecuteRun` therefore appends the `fs_*` tool names to `runSpec.AllowTools`
whenever `FolderScopes` is non-empty (three lines next to
`app_run.go:857-862`); the folder matcher is the finer gate for those
tools anyway. Without this, "scoped + restricted" denies every file
operation and ends the turn on the first one.

---

## 5. Batch semantics recommendation

**Best-effort with per-item results, preceded by a pure preflight, with
the FILE as the atomicity unit. Not all-or-nothing.** This is validated
against precedent, not merely preferred:

- `multiedit` is the tree's only batch tool and is best-effort: it applies
  what it can, reports `EditsFailed` with index + reason + the offending
  edit (`multiedit.go:44-48`, `:133-148`), and marks the WHOLE response as
  an error only when nothing was applied (`:285-297`). Its one
  all-or-nothing check is a pure structural preflight (`validateEdits`,
  `:118-126`) — the same two-phase shape proposed here.
- The tool error contract's retry-invariance criterion (`tools.go:100-112`)
  makes a scope miss, a not-found `old_string`, an overlapping range or a
  non-existent file all level-1: correctable per item. A missing session
  id or a history-DB failure stays level-3 for the whole call, exactly as
  in `write.go:165-171` — batching does not change that.
- `runallowlist.go`'s "matcher is total; deny is the safe direction"
  argues for the preflight: all scope decisions are computed BEFORE any
  I/O, so a batch that is entirely out of scope does nothing, and a
  partially out-of-scope batch never writes a denied item. The preflight
  cannot promise more than that: a mid-batch I/O failure (read-only file,
  disk full on item 12 of 20) still leaves items 1–11 on disk, and
  reporting that truthfully is the only honest option — a rollback
  (journal + rename cascade across files) is out of scope and would not
  cover the case where the process dies anyway.
- Why not all-or-nothing: one bad path would discard N−1 valid writes and
  force the model to resend the full batch (doubling token spend on large
  writes), and since the protocol carries exactly one result per call
  (section 1, `fantasy/agent.go:760-776`), a partial outcome that is NOT
  reported per item is a lie about disk state. Best-effort + per-item
  report is the only policy under which the model's view equals the disk.
- Per-file atomicity: items targeting the same file (`fs_replace`,
  `fs_write_lines`) are grouped, applied in memory, and written once with
  `fsext.AtomicWriteFile` — one history version per file per call, as
  `multiedit` does. Within a file, a failed item does not block the
  others (multiedit precedent), except that overlapping ranges in
  `fs_write_lines` fail BOTH overlapping items.
- Whole-response `IsError` iff zero items succeeded (multiedit precedent);
  `StopTurn` is never set by scope denials. A `permissions.Request`
  denial (interactive user, or a restricted-run miss) still returns
  `NewPermissionDeniedResponse()` for the whole call, as every write tool
  does today. Each denied item is also `slog.Warn`ed with `tool`,
  `session_id`, `path`, `op` — `loggedTool` (`logged_tool.go:135-143`)
  only logs whole-response errors, so partial denials would otherwise be
  invisible to the operator.
- Caps (per call): 50 items; 200 KB total read output (`MaxViewSize`
  reuse); `context_lines` ≤ 50; grep matches ≤ 100 per item (existing
  limit). Over-cap input is a whole-call level-1 error before the
  preflight (shape, not policy).

### 5.1 Representative sketch — batched write

```go
type FSWriteItem struct {
	Path       string `json:"path" description:"File path, absolute or relative to the working directory"`
	Content    string `json:"content" description:"Complete new content of the file"`
	CreateOnly bool   `json:"create_only,omitempty" description:"Fail this item if the file already exists (create, never overwrite)"`
}

type FSWriteParams struct {
	Items []FSWriteItem `json:"items" description:"Files to write. Each item is validated and reported independently; existing files are overwritten unless create_only is set"`
}

// Per-item outcome, shared by every fs_* tool (rendered into Content for
// the model and JSON-encoded into Metadata for hosts/UIs).
type FSItemResult struct {
	Index     int    `json:"index"`
	Path      string `json:"path"`             // as the model sent it
	Op        string `json:"op,omitempty"`     // resolved FileOp ("create" vs "overwrite")
	Status    string `json:"status"`           // "ok" | "denied" | "failed" | "skipped"
	Error     string `json:"error,omitempty"`  // level-1 reason
	Additions int    `json:"additions,omitempty"`
	Removals  int    `json:"removals,omitempty"`
	Diff      string `json:"diff,omitempty"`   // write-side only; capped
}

type FSBatchResponseMetadata struct {
	Tool      string         `json:"tool"`
	Succeeded int            `json:"succeeded"`
	Failed    int            `json:"failed"`
	Items     []FSItemResult `json:"items"`
}
```

Model-facing `Content` (deterministic, greppable, one line per item, file
blocks only for reads):

```
fs_write: 2 of 3 items ok
[0] ok      src/a.go (overwrite, +3/-1)
[1] ok      src/new/b.go (create)
[2] denied  ../etc/hosts: outside every scope
```

For `fs_read` the block form is `<file path="…" lines="120-160" status="ok">…numbered lines…</file>`
per item (the `<file>` wrapper the model already knows from `view`,
`view.go:287-294`), and for `fs_grep` with `context_lines` one
`<match path="…" lines="115-125" hit="120">` block per merged hit
window. Every item's `Path` is echoed exactly as sent so the model can
correlate.

Read-before-write for `fs_replace`/`fs_write_lines` follows `edit`
(`edit.go:307-310`): an item on a file never read in this session (per
`filetracker`) fails with "read it first" — and when the scope grants
`replace` but not `read`, that item can never succeed; the message says
so explicitly rather than pretending the file is unreadable.

---

## 6. SDK surface sketch

Naming follows the existing per-call fields on `RunOverrides`
(`AllowBash`, `AllowTools`, `DisableSubAgents`, `ModelRole`,
`TimeoutOptionsSet`; `app_run.go:75-176`):

```go
// app.RunOverrides (internal/app/app_run.go)
	// FolderScopes hands THIS invocation a scoped, batch-capable file
	// toolset instead of the default coder toolset: the fs_* family
	// (list/find/grep/read/write/write_lines/replace/delete), each item
	// checked against these entries, and NO bash/run_command/download/
	// git_read/agentic_fetch/MCP tools unless RestrictedRun with at least
	// one AllowBash pattern keeps the command tools (see permission.
	// FolderScopeSpec.KeepCommandTools). Entries are resolved against the
	// App's working directory; a malformed entry fails ExecuteRun. NOT
	// persisted on the session (unlike SmartModel/SystemPrompt/
	// ReasoningEffort): scoping is a per-call policy, and a later run on the
	// same session without it is unscoped. Refused (error before any
	// provider traffic) when the resolved smart/worker provider is a CLI
	// provider, whose tools run outside this process. Fork patch (SDK
	// scoped file tools).
	FolderScopes []permission.FolderScopeEntry
```

```go
// sdk (sdk/sdk.go) — aliases, zero conversion code, matching the
// CredentialSet pattern (sdk.go:78-87)
type (
	FolderScope = permission.FolderScopeEntry
	FileOp      = permission.FileOp
)

const (
	FileOpList       = permission.FileOpList
	// … one constant per op
)
```

Host call:

```go
res, err := client.Run(ctx, sdk.RunRequest{
	Prompt:            "…",
	ContinueSessionID: id,
	Overrides: sdk.RunOverrides{
		FolderScopes: []sdk.FolderScope{
			{Dir: "src", Ops: []sdk.FileOp{sdk.FileOpList, sdk.FileOpFind, sdk.FileOpGrep, sdk.FileOpRead, sdk.FileOpReplace, sdk.FileOpWriteLines}},
			{Dir: "src/secrets"},                                   // carve-out: nothing
			{Dir: "docs", Ops: []sdk.FileOp{sdk.FileOpRead, sdk.FileOpCreate, sdk.FileOpOverwrite}},
		},
	},
})
```

Threading, step by step (every step is an existing seam):

1. `ExecuteRun` (`app_run.go:594-636`), right where `callOpts` is built:
   ```go
   var scope *permission.FolderScope
   if len(overrides.FolderScopes) > 0 {
   	compiled, err := permission.BuildFolderScope(permission.FolderScopeSpec{
   		WorkingDir:       app.config.WorkingDir(),
   		Entries:          overrides.FolderScopes,
   		KeepCommandTools: runSpecRestricted && len(runSpec.AllowBash) > 0, // computed from the merged runSpec below
   	})
   	if err != nil {
   		return nil, fmt.Errorf("invalid folder scopes: %w", err) // hard error, unlike BuildRunAllowlist's log-and-drop
   	}
   	scope = &compiled
   }
   callOpts := &agent.CallOptions{ …, FolderScope: scope }
   ```
   Hard error rather than `BuildRunAllowlist`'s "drop the bad pattern,
   arm the rest" (`runallowlist.go:148-176`): dropping a bad GRANT
   narrows (safe), but dropping a bad CARVE-OUT widens — the compiler
   cannot tell which the host meant, so it refuses. The run allowlist's
   merge order means `runSpec` must be assembled before `callOpts`; today
   it is assembled later (`:857-862`) — a small reorder inside
   `ExecuteRun`, and the `fs_*` names are appended to `runSpec.AllowTools`
   in the same block (section 4).
2. `agent.WithCallOptions(ctx, callOpts)` — unchanged; `buildCall`/
   `runInternal` already stamp `CallOptions` onto the call
   (`coordinator_run.go:110-123`, `:276`).
3. `resolveSessionModels` / `applyModelOverrides` /
   `resolveCredentialsModels` → the CLI-provider refusal (3.5) →
   `pinCallTools(ctx, cfg)` → `buildTools(ctx, …)` →
   `applyCallFolderScope` + `tools.NewFSTools(…, scope)` (3.4) →
   `resolvedOverrides.tools` → `pin` → `SessionAgentCall.Tools` → the turn.
4. Sub-agents spawned by the `agent` tool inside the call are built from
   the same ctx (3.4 step 3), so they inherit the scoped family with no
   extra plumbing; `InheritSessionRunAllowlist` stays as is.
5. `sdk/README.md` gains a subsection under the trust-model text: scoping
   is per-call, not per-session; not persisted; the durable-restart caveat
   until T12 lands; the CLI-provider refusal.

No CLI flag and no `rush.json` key in v1 (section 7).

---

## 7. Open questions / non-goals

Deliberately unresolved:

- **Durable-restart persistence of the scope** (3.5): depends on the shape
  the r4-1-2-3 worker lands for the `RunAllowlist` axis. Until then a
  scoped SDK turn that is orphaned into the durable queue restarts
  unscoped — same class as F2, must be closed before the feature is
  called done, and must reuse whatever mechanism that work introduces
  rather than inventing a parallel one.
- **cliprovider enforcement** (3.5): v1 refuses; wiring the scope into
  `newRushMCPServer`'s five tools is a follow-up. Note the MCP tools'
  fixed `"cli-mcp"` session id (`mcpserver_tools.go:526`) also means the
  per-session run allowlist never applies to them today — a pre-existing
  gap adjacent to this work, not caused by it.
- **`filetracker.relpath` uses `os.Getwd()`** (`filetracker/service.go:61-74`),
  not the App's working directory — an existing SDK wart (`sdk.Open`
  never chdirs, `sdk.go:14`) that the read-before-write rule inherits.
  Not fixed here; flagged because a host whose process cwd differs from
  `WorkingDir` may see spurious "read it first" failures, and that will
  look like a scoping bug.
- **Overlap semantics choice** (longest-dir-wins with empty-`Ops`
  carve-outs) is a design decision the operator should confirm; the
  alternative (union) is simpler but cannot exclude a sub-folder.
- **`fetch` and `file://`**: the network tools are kept in scoped calls;
  a one-line test that `fetch` rejects `file://` URLs through the SSRF
  guard belongs in T8's test list. Not verified in this pass.

Non-goals for v1 (explicitly out of scope):

- Retrofitting arrays or scope checks onto the legacy `view`/`glob`/
  `grep`/`ls`/`write`/`edit`/`multiedit` tools; the CLI/web paths keep
  them unchanged.
- Directory deletion, recursive delete, rename/move, chmod, binary/image
  reads in `fs_read` (item error: "use an unscoped call"), non-recursive
  ("this folder only, not subfolders") scope entries.
- A `stop_on_error`/all-or-nothing batch mode; rollback/journaling.
- `rush run --scope …` CLI flag and a `rush.json` `permissions.scopes`
  key; interactive web sessions.
- Web UI rendering of the `fs_*` metadata (the UI will show the raw text;
  `FSBatchResponseMetadata` is designed so it can be rendered later).
- Making `KeepCommandTools` airtight (OS sandboxing).
- Any change to the `RunAllowlist` matcher, `permission.Request`, or the
  R3-4 arming/clearing — nothing in this design touches them.

---

## 8. Suggested task breakdown

Names + one-line scope, in dependency order. Tests are scoped to the
package each task touches; the full suite stays with the orchestrator.

| # | Task | Scope | Depends on |
|---|---|---|---|
| T1 | `permission/folderscope.go` matcher | `FileOp`, `FolderScopeEntry`, `FolderScopeSpec`, `BuildFolderScope` (hard error on bad entry), `FolderScope.Grants/Check/Roots/KeepsCommandTools`, `ScopeDeniedError`; tests: longest-match, carve-out, `..`, cross-volume, Windows case, zero value denies all | — |
| T2 | `tools/fs_scope.go` resolver | `resolveScopedPath` (SmartJoin → Abs → Clean → EvalSymlinks of longest existing prefix); tests: symlink escape denied, non-existent create path judged by parent, resolution error denies | T1 |
| T3 | `tools/fs_batch.go` runner | shared preflight → group-by-file → execute → render (`FSItemResult`, `FSBatchResponseMetadata`, text layout), caps, per-denied-item `slog.Warn`, whole-call `IsError` iff nothing succeeded | T1, T2 |
| T4 | `fs_read`, `fs_list`, `fs_find` | read-side tools reusing `readTextFile`/`ListDirectoryTree`/`globFiles`; range + centre/radius item forms; result post-filtering; no `permissions.Request`; `error_contract_test.go` registration | T3 |
| T5 | `fs_grep` with `context_lines` | extend `searchWithRipgrep` (`-C N`, parse `"context"` messages) and `searchFilesWithRegex`/`fileMatches` (ring buffer + read-ahead); merged hit windows; tests exercise the fallback path (`getRg` returns "" under `testing.Testing()`, `rg.go:17-19`), ripgrep path needs a build-tagged/manual check | T3 |
| T6 | `fs_write`, `fs_replace`, `fs_write_lines`, `fs_delete` | write-side tools reusing `findAndReplace`/`commitFileChange`-style helper, `CheckForbiddenWrite`, CRLF, history versions, `filetracker`; one `permissions.Request` per call; per-file atomic writes; overlap rejection + bottom-up apply; delete files only; `error_contract_test.go` registration | T3 |
| T7 | `config.allToolNames` / `resolveReadOnlyTools` | add the 8 `fs_*` names, 4 read-side ones to the read-only list; existing config tests | T4–T6 (names fixed) |
| T8 | Coordinator per-call wiring | `CallOptions.FolderScope`; `applyCallFolderScope` (ordering after worker layering, escape-hatch strip, `KeepCommandTools`, `AllowedMCP = {}`); `NewFSTools` construction in `buildTools`; tests mirroring `coordinator_tool_pinning_test.go`: scoped vs concurrent unscoped call, `UpdateModels` never publishes `fs_*`, worker layering cannot re-add `bash`, sub-agent inside a scoped call gets read-side `fs_*` only, `fetch` rejects `file://` | T7 |
| T9 | CLI-provider refusal | shared helper in the three per-call resolvers; test with a `Type: "cli"` provider config | T8 |
| T10 | App + SDK surface | `RunOverrides.FolderScopes`; `ExecuteRun` compile (hard error), `runSpec` reorder + `fs_*` → `AllowTools` under `RestrictedRun`, `KeepCommandTools` derivation; sdk aliases; README section; sdk tests in library mode with a scripted provider: out-of-scope item denied per item while in-scope items succeed, scoped and unscoped runs concurrent on two sessions (the `sdk_concurrent_policy_test.go` shape) | T8, T9 |
| T11 | Prompt hint (optional) | conditional block in `coder.md.tpl` for scoped mode (the template names `edit`/`multiedit`/`write` at `:17`); via the per-call `prompt.Build` path | T10 |
| T12 | Durable-restart persistence of the scope spec | carry `FolderScopeSpec` in `SessionAgentCallData`, recompile in the pump; **blocked on r4-1-2-3 landing** — reuse its mechanism | r4-1-2-3, T10 |

T4, T5, T6 can proceed in parallel once T3 exists.
