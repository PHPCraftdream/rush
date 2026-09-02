# SDK/library review — round 5 (23:31)

Date: 2026-09-02 23:31 Europe/Berlin
Reviewed repository state: `be8369b11146cc2a98eb94599ab8ba0d24d71ef4`
Round-4 baseline: `ddaccc1a621002f61e54a7f042a7215f9ee6050e`
Method: static, read-only review of committed source, diffs, tests, and public
SDK documentation. The main worktree was not modified. No test command was run
because other agents were working in parallel and the requested mode was
read-only.

## Verdict

The new SDK surface is moving in the right architectural direction. The
folder-scoped `fs_*` family is isolated per call, `DiskProvider` reaches all
eight tools and their path resolver, CLI-backed models are rejected, and
non-serializable providers are prevented from entering the durable queue. The
round-4 restricted-run findings are also fixed for newly written durable rows.

However, the folder scope is not yet a dependable security boundary at
`be8369b1`. Two P0 paths can widen access:

1. a failed per-call tool build silently falls back to the shared, normally
   unscoped toolset;
2. scope roots are matched in lexical form while requested paths are matched
   after symlink resolution, allowing a nested deny carve-out to be bypassed.

There are also material durable-replay and SDK-contract gaps: replay restores
only the folder-scope part of `CallOptions`, documented sentinel errors are not
exported from `sdk`, `fs_write` can overwrite after failing to read the old
content, and ephemeral library mode has no logical filesystem root for relative
SDK paths.

Release recommendation: do not advertise `FolderScopes`/`DiskProvider` as an
isolation or ACL boundary until R5-1 and R5-2 are fixed and covered end to end.
R5-3 through R5-6 should be closed before declaring the library SDK stable.

## Reviewed change set

The focused range contains 25 commits and about 15,915 added lines across 112
files (`ddaccc1a..be8369b1`). The relevant groups are:

| Commits | Area | Review disposition |
| --- | --- | --- |
| `271550b1` | Persist/rebind restricted-run policy by logical call | Correctly closes R4-1 through R4-3 for new rows. |
| `232e62c4`–`1b6b2b0e` | Folder matcher, resolver, batch runner, eight `fs_*` tools, per-call SDK scope, CLI refusal, durable scope replay | Strong structure, but R5-1, R5-2, R5-3, and R5-6 remain. |
| `dfce5ac7`–`eff3ce15` | `DiskProvider`, tool wiring, SDK exposure, durable refusal | Provider routing is broad and fail-closed at queue boundaries; R5-1, R5-2, R5-5, and R5-6 remain. |
| `53aa30d9`, `cec10b5d` | Test/logger and runtime leak/hang fixes | Reviewed for interaction with SDK lifecycle; no new blocker found in this pass. |
| `b7563747`, `f4e30ce4`, `be8369b1` | `/wrush`, accumulated docs, formatting | Not central to the SDK execution contract; `f4e30ce4` closes R4-4. |

## Findings

### R5-1 — P0: scoped calls fail open to the shared unscoped toolset

`pinCallTools` is responsible for building the tool slice from the current
call's `CallOptions`. Its contract explicitly returns `nil` on a missing coder
configuration, a `buildTools` error, or a ready-gate error, and logs that the
call will fall back to the shared toolset
(`internal/agent/coordinator_models.go:480-511`).

Both ordinary model resolution and credential-backed override resolution store
that result without an error channel
(`internal/agent/coordinator_models.go:249-258`, `404-409`).
`resolvedOverrides.pin` copies tools only when the result is non-nil
(`internal/agent/coordinator_models.go:416-433`). At turn start, nil means
`a.tools.Copy()`, i.e. the process-shared legacy toolset
(`internal/agent/agent_turn.go:278-287`).

This fallback predates folder scopes, but its meaning changed. For a call with
`FolderScopes` or `DiskProvider`, the per-call tool build is the mechanism that
removes legacy file tools, MCP tools, and usually command tools. Falling back to
the shared slice can therefore reintroduce `view`, `write`, `edit`, `glob`,
`grep`, `ls`, or a configured sub-agent. An unrestricted SDK run is
auto-approved, so the restored tools are not rescued by the run allowlist.

The durable scoped path already recognizes the invariant: if `pinCallTools`
returns nil, `RunSessionAgentCall` refuses the row rather than using shared
tools (`internal/agent/coordinator_interrupt.go:648-669`). The ordinary
in-process path needs the same rule.

Required fix:

- change `pinCallTools` to return `([]fantasy.AgentTool, error)` or another
  result that distinguishes "legacy fallback permitted" from "policy build
  failed";
- whenever `CallOptions.FolderScope != nil` or `DiskProvider != nil`, fail the
  run before provider traffic if the pinned toolset cannot be built;
- legacy unscoped callers may retain the historical fallback if compatibility
  requires it.

Required regression: inject both `buildTools` and ready-gate failures into a
scoped SDK call and prove that no model request is sent and no shared tool is
offered. Cover `Run` and `RunWithCredentials`.

### R5-2 — P0: symlink resolution can bypass a nested deny carve-out

The matcher requires callers to supply already-resolved absolute paths
(`internal/permission/folderscope.go:28-33`). Yet production compilation only
joins relative entries to `WorkingDir` and calls `filepath.Clean`; it never
resolves symlinks (`internal/permission/folderscope.go:149-178`, called from
`internal/app/app_run.go:656-668`).

Every requested item takes the opposite path: `resolveScopedPath` finds the
longest existing prefix and calls the active provider's `EvalSymlinks` before
the matcher sees it (`internal/agent/tools/fs_scope.go:13-65`). Matcher roots
and item paths can therefore be in different namespaces.

Concrete bypass:

1. `/work` grants `read`;
2. `/work/alias` is a deny carve-out (empty `Ops`);
3. `/work/alias` is a symlink to `/work/private`;
4. reading `/work/alias/key` is resolved to `/work/private/key`;
5. the lexical carve-out `/work/alias` no longer matches, while the broader
   `/work` grant does.

The tests conceal this mismatch. `fsWriteTestScope` resolves every entry using
`resolveScopedPath` before calling `BuildFolderScope`, explicitly so matcher
roots and item paths are both resolved
(`internal/agent/tools/fs_write_test.go:15-33`). Production does not perform
that preparatory step. SDK tests also call `evalTempDir`, so the working root is
already canonical before scope compilation (`sdk/sdk_folder_scope_test.go:95`).

This applies to custom `DiskProvider` implementations too: entries are compiled
without consulting the provider, but requested paths use the provider's
`EvalSymlinks` implementation.

Required fix:

- canonicalize every scope entry with the same `DiskProvider` and the same
  longest-existing-prefix algorithm used for item paths before compiling it;
- either persist canonical roots, or persist the raw spec and repeat
  provider-appropriate canonicalization during rebuild;
- preserve the zero-value deny-everything behavior on any resolution error.

Required regressions: a real symlinked deny carve-out under a broader grant, a
symlinked `WorkingDir`, and equivalent cases through a fake `DiskProvider`.

Residual boundary to document: path-check-then-operation remains susceptible
to a hostile local process swapping symlinks between those operations. If the
threat model excludes concurrent host filesystem mutation, say so explicitly;
otherwise handle-relative filesystem primitives are needed.

### R5-3 — P1: durable folder-scope replay drops the rest of `CallOptions`

`SessionAgentCall.CallOptions` is process-local and excluded from JSON
(`internal/agent/agent.go:366-374`). The durable DTO gained
`FolderScopeSpec`, but no serializable fields for the other execution-policy
parts of `CallOptions` (`internal/session/session_runqueue.go:108-194`).

On rebuild, the code constructs exactly:

```go
rebuiltCallOptions = &CallOptions{FolderScope: &compiledScope}
```

(`internal/agent/coordinator_interrupt.go:567-575`). This loses at least:

- `DisableSubAgents`;
- `ModelRole` used while rebuilding sub-agents;
- `TimeoutOptionsSet`, `TimeoutExtendsOnProgress`, and `TimeoutHardCap`.

`MaxCost` and `MaxTokens` are safe because they are persisted separately on the
call. `DiskProvider` is deliberately refused before durable enqueue. The fields
above have neither protection.

The most important policy regression is `DisableSubAgents=true`: a scoped
toolset intentionally leaves the conversation/delegation decision separate
from file operations. After replay, the rebuilt options no longer ask
`buildTools` to strip `agent`/`agentic_fetch`, so a call declared single-agent
can regain delegation. Watchdog and role behavior can also differ from the
original run.

Required fix: define a versioned serializable durable execution-options DTO, or
explicit durable fields, and reconstruct all replay-relevant options together.
Do not treat successful scope restoration as restoration of the whole call
policy.

Required regression: round-trip a call combining `FolderScopes`,
`DisableSubAgents`, an explicit all-zero timeout policy, and a non-default role;
then inspect the rebuilt toolset and watchdog/role inputs.

### R5-4 — P1: documented SDK sentinel errors cannot be referenced externally

The public package exports `sdk.ErrClientClosed`, but it does not export the
other sentinels consumers are told to classify (`sdk/sdk.go:153-159`). Public
documentation says a busy call wraps `agent.ErrSessionBusy` and recommends
`errors.Is` (`sdk/sdk.go:465-469`, `sdk/README.md:308-313`). The DiskProvider
section similarly names `agent.ErrDiskProviderNotDurable`
(`sdk/README.md:292-302`).

An external module cannot import `github.com/PHPCraftdream/rush/internal/agent`
because Go's `internal` visibility rule forbids it. Repository tests can import
that package because they live under the parent tree, so they do not validate
the consumer-facing contract.

Required fix: re-export stable aliases from `sdk`, for example:

```go
var (
    ErrSessionBusy            = agent.ErrSessionBusy
    ErrDiskProviderNotDurable = agent.ErrDiskProviderNotDurable
)
```

Then make all public docs refer only to `sdk.ErrSessionBusy` and
`sdk.ErrDiskProviderNotDurable`. Add a compile test in a temporary external Go
module that imports only the public SDK.

### R5-5 — P1: `fs_write` overwrites after failing to read existing content

For an existing file, `fs_write` tries to capture its old content, but ignores
every `DiskProvider.ReadFile` error and leaves `oldContent` as an empty string
(`internal/agent/tools/fs_write.go:218-224`). It then applies the requested
items and calls `commitFileChange` with that false empty baseline
(`internal/agent/tools/fs_write.go:252-279`).

With a transient custom-provider failure, a permission race, or another read
error after `Stat` reported the file exists, the tool can still overwrite the
file. Its diff describes creation from empty content, and its history/undo
baseline is wrong. This converts an observability failure into destructive data
loss and corrupts the audit trail.

Required fix: treat failure to snapshot an existing file as a pre-write error.
Classify it using the same fatal/per-item rules as the other mutation tools, but
never call `WriteFile` or record history without a trustworthy old value.

Required regression: a fake provider where `Stat` says the file exists,
`ReadFile` fails, and `WriteFile` would succeed; assert zero writes and zero
history entries.

### R5-6 — P1: ephemeral library mode has no logical filesystem root

The most library-native mode deliberately accepts an empty `WorkingDir` and
uses an in-memory database (`sdk/library_mode.go:63-113`). The same empty value
is passed into the app's config store. Folder scopes, however, reject every
relative entry when `WorkingDir` is empty
(`internal/permission/folderscope.go:145-167`). The README's natural SDK
example, `{Dir: "src", ...}`, therefore cannot be used unchanged in ephemeral
library mode.

Supplying an absolute virtual scope only partly works. Relative item paths are
joined to the empty working directory and then passed to `filepath.Abs`, which
binds them to the Rush host process's current directory
(`internal/agent/tools/fs_scope.go:27-30`). A custom virtual filesystem thus has
no stable logical root unless the model always emits absolute paths. Changing
the host process CWD can change resolution for the same SDK request.

All current DiskProvider SDK tests open application mode with a real resolved
working directory (`sdk/sdk_disk_provider_test.go:207`, `273`, `322`, `362`).
The missing `ModeLibrary` + empty `WorkingDir` combination is not covered.

Required fix: separate the logical filesystem root from the persistence/data
directory. Expose a stable workspace root for ephemeral library clients (or
synthesize a documented virtual root), and use it consistently for scope-entry
and item-path resolution. If no root exists, reject relative item paths with an
explicit SDK error rather than consulting process CWD.

Required regression: `ModeLibrary`, empty persistence `WorkingDir`, a custom
provider, a relative folder scope, and relative model paths, with no host disk
access.

### R5-7 — P2: durable rebuild discards the persisted request origin

`SessionAgentCallData.Origin` explicitly promises to preserve the entry channel
across processes (`internal/session/session_runqueue.go:177-181`). Both generic
conversion functions copy it (`internal/agent/call_data_conversion.go:143-161`,
`184-200`). But the coordinator's actual pump rebuild constructs a fresh
`SessionAgentCall` without assigning `Origin`
(`internal/agent/coordinator_interrupt.go:578-611`).

The replayed call therefore gets the zero/unspecified origin despite carrying a
valid persisted value, making audit and transport metadata disagree with the
request that entered the queue.

Required fix: set `Origin: data.Origin` in `RebuildSessionAgentCall` and add a
rebuild regression for every non-zero origin used by SDK/CLI/server entry
points.

### R5-8 — P2: the public implementation comment contradicts shipped behavior

`CallOptions.DiskProvider` still says durable refusal is a "LATER task, not
implemented here" (`internal/agent/call_options.go:113-118`). Commit
`eff3ce15` implements producer refusal, a durable marker, and consumer refusal.
The stale statement is likely to mislead the next maintainer reviewing exactly
this safety boundary.

Required fix: update the comment to name the implemented three-layer refusal
and its durable marker, ideally linking to the committed design document rather
than a task sequence.

## Round-4 disposition

| Round-4 finding | Status at `be8369b1` | Evidence |
| --- | --- | --- |
| R4-1: queued calls collapse to the last session policy | **Closed for new rows** | `RunAllowlistSpec` is serialized per call and rebound by `LogicalCallID`; the two-policy regression exercises opposite policies. |
| R4-2: baseline-governed parent gives child auto-approve without restriction | **Closed for new rows** | rebuilt calls regain their own active policy and inheritance resolves the effective parent policy; a durable sub-agent regression was added. |
| R4-3: policy/auto-approve disappear on process restart | **Closed for new `ExecuteRun` rows** | the persisted spec recompiles the gate and acts as the marker to re-arm non-interactive auto-approval. Pre-migration rows intentionally retain legacy fallback behavior. |
| R4-4: committed source references an absent review | **Closed** | `f4e30ce4` commits `docs/reviews/2026-09-01-sdk-review-fh.md`. |

The migration qualification matters: nil specs from pre-migration rows are
still accepted and follow the documented legacy fallback. That is a conscious
compatibility choice, not proof that old queued rows acquire the new policy.

## Positive observations

- The `fs_*` family has one shared batch contract, per-item scope decisions,
  bounded batch/output behavior, and filtering of list/find/grep results rather
  than checking only the requested root.
- `resolveScopedPath` fails closed on unreadable/ambiguous prefixes and resolves
  a non-existent tail through its existing parent, which is the right direction
  for create operations.
- `DiskProvider` covers stat, symlink resolution, streaming/whole-file reads,
  writes, delete, list, find, and search; the default OS implementation remains
  available for decoration through `sdk.OSDiskProvider()`.
- A provider requires a folder scope, and command tools cannot coexist with a
  custom provider, preventing a mixed virtual/real filesystem turn in the
  normal successful-build path.
- CLI-provider-backed scoped runs fail before model traffic because their
  subprocess tools cannot honor in-process scope checks.
- Durable provider safety is layered: producers refuse queueing, the DTO keeps
  a marker, and the consumer refuses marked rows instead of substituting the OS
  filesystem.
- The folder-scope spec and run-allowlist spec now survive the JSON boundary;
  corrupt scope recompilation yields the zero deny-everything matcher.
- The R4 fix added targeted two-policy, sub-agent inheritance, JSON round-trip,
  and restart behavior tests rather than only changing comments or state shape.

## Recommended fix order

1. Make every scoped/provider-backed tool build fail closed (R5-1).
2. Canonicalize scope roots with the active provider before matching (R5-2).
3. Preserve the complete replay-relevant execution policy, especially
   `DisableSubAgents` and explicit timeout presence (R5-3).
4. Export usable SDK sentinel errors and validate from an external module
   (R5-4).
5. Refuse `fs_write` when the pre-write snapshot cannot be read (R5-5).
6. Add a logical workspace root independent of library persistence (R5-6).
7. Restore durable `Origin` and update stale comments (R5-7/R5-8).

After those changes, run focused race-enabled tests for `internal/agent/tools`,
`internal/agent`, `internal/app`, and `sdk`, followed by `go test -race ./...` in
an isolated worktree. Include external-module compilation and real symlink
tests; same-package unit tests currently mask both public importability and the
production root-canonicalization mismatch.

## Scope and validation limits

This review is anchored to committed HEAD `be8369b1`. The main worktree had an
unrelated deletion of `web/dist/.gitkeep`; it was neither used as review input
nor modified. No source files, generated assets, configuration, or test state
were changed. Findings are based on static control-flow, serialization, path
resolution, API visibility, and test-oracle analysis; runtime confirmation is
still required after fixes are implemented.
