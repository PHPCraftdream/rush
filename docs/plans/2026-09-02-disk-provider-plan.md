# Pluggable disk provider for the scoped `fs_*` tool family — design plan

Date: 2026-09-02. Status: **planning only** — no code written, nothing
built, nothing run. Read-only investigation of the working tree at
`4f56929f` plus the uncommitted T12 changes (`internal/session/session_runqueue.go`,
`internal/agent/call_data_conversion.go`, `internal/agent/coordinator_interrupt.go`,
`internal/agent/call_options.go`, `internal/app/app_run.go` and the two
untracked T12 test files). Every claim cites `file:line` so it can be
re-checked rather than believed.

Covers session tasks **#856 → #859**: a caller-supplied Go value that
replaces the real filesystem for ONE agent turn's `fs_*` tools.

---

## 0. Executive shape

| Task | What lands | New files | Existing files touched |
|---|---|---|---|
| #856 | `tools.DiskProvider` interface + value types + `tools.OSDisk()` default | `fs_provider.go`, `fs_provider_os.go`, `fs_provider_test.go` | **none** |
| #857 | Every `os.*`/`filepath.*`/search call in `fs_scope.go` + the 8 `fs_*.go` routed through the provider; constructors gain a trailing `disk` parameter (nil ⇒ real disk) | 1 fake-provider test file | 10 in `tools` (incl. **`edit.go`** and **`view.go`** — see §4.3) |
| #858 | `CallOptions.DiskProvider`; `buildTools` threads it into the 8 constructors | 1 test file | `call_options.go`, `coordinator_tools.go` |
| #859 | `RunOverrides.DiskProvider`, SDK aliases, README; **durable-restart refusal** | 2–3 test files | `app_run.go`, `sdk/sdk.go`, `sdk/README.md`, + the durable-refusal sites in §7 |

The single most important design decision in this document is **§7**: a
disk provider is arbitrary Go code and cannot be persisted, so a call
carrying one must be **refused at the durable-queue boundary** (T9's
"refuse outright" precedent), *not* persisted-and-restored (T12's
precedent). Getting this wrong means a host's in-memory-only replay
lands on the operator's real disk after an unannounced restart.

---

## 1. Inventory — every real OS interaction the `fs_*` family makes

This is the ground truth the interface in §3 is derived from. It was
produced by reading each call site, not by grepping for `os.`.

### 1.1 `internal/agent/tools/fs_scope.go` — `resolveScopedPath` (:27-69)

| Line | Call | Kind |
|---|---|---|
| :28 | `filepathext.SmartJoin(workingDir, raw)` | pure string (`internal/filepathext/filepath.go:11-16`) |
| :28 | `filepath.Abs(...)` | reads the **process cwd** only when the joined path is relative |
| :32 | `filepath.Clean` | pure |
| :38 | `os.Stat(prefix)` — in a loop walking up to the longest existing prefix | **OS** |
| :50 | `filepath.Dir` | pure |
| :57 | `filepath.EvalSymlinks(prefix)` | **OS** (lstat + readlink loop) |
| :64 | `filepath.Rel` | pure |
| :68 | `filepath.Join` | pure |

So `resolveScopedPath` needs exactly **two** provider primitives: `Stat`
and `EvalSymlinks`. `filepath.Abs` is deliberately left out of the
interface — see §3.4(e).

Fail-closed contract that must survive the abstraction: the loop breaks
on `errors.Is(statErr, fs.ErrNotExist)` (:47) and errors out on anything
else. A provider whose "missing" error does not satisfy
`errors.Is(err, fs.ErrNotExist)` turns every create into a hard resolve
failure.

### 1.2 Read side

| Tool | Site | Real call |
|---|---|---|
| `fs_read` | `fsReadOne` `fs_read.go:156` → `readTextFile` `view.go:343-398` | `os.Open` (`view.go:344`), then bounded `bufio` line reads. **Never** loads the whole file — a 2 GiB file read at `start_line 1..10` is cheap today. |
| `fs_list` | `fsListOne` `fs_list.go:116` | `os.Stat(absPath)` |
| `fs_list` | `fs_list.go:125` | `fsext.ListDirectory(absPath, ignore, depth, maxFiles)` → `fastwalk.Walk` + per-directory `.gitignore`/`.rushignore` `os.ReadFile` (`internal/fsext/ls.go:274-323`, `:169-188`), plus once-per-process global git-excludes / `~/.config/rush/ignore` reads (`:87-124`) |
| `fs_list` | `fs_list.go:136,146` | `filepath.FromSlash` then `createFileTree`/`printTree` — pure, **but** `createFileTree` decides file-vs-directory from a **trailing native separator** (`ls.go:206`) and does `strings.TrimPrefix(path, rootPath)` (`ls.go:176`). The `List` contract must preserve both. |
| `fs_find` | `fsFindOne` `fs_find.go:106` → `globFiles` `glob.go:89-101` | `getRgCmd` → `exec.LookPath("rg")` (`rg.go:18-30`) + **`rg --files -L --null` subprocess** (`glob.go:90-98`, `runRipgrep` `glob.go:103-132`, `cmd.CombinedOutput()`); on any rg failure falls back to `fsext.GlobGitignoreAware` (`glob.go:100`, `internal/fsext/fileutil.go:94-96`) → `fastwalk.Walk` + doublestar |
| `fs_grep` | `fsGrepRunItem` `fs_grep.go:129` → `fsGrepSearchContext` `fs_grep.go:222-234` | **engine A**: `rg --json -H -n -0 <pat> [-C N] [--glob inc] <path>` (`rg.go:62-74`), `appendRgIgnoreFiles` first `os.Stat`s `.gitignore`/`.rushignore` (`rg.go:79-87`), then `StdoutPipe`/`Start`/parse/drain/`Wait` (`fs_grep.go:256-281`). **engine B** (fallback, and *always* under `go test` because `getRg()` returns `""` when `testing.Testing()`, `rg.go:19-21`): `fsext.NewFastGlobWalker` + `filepath.Walk` (`fs_grep.go:339-341`), `isTextFile` → `os.Open` + 512-byte sniff (`grep.go:616-639`), `os.Open` + bounded line scan (`fs_grep.go:393`, `readBoundedLine` `grep.go:591-613`) |
| `fs_grep` | `fsGrepRender` `fs_grep.go:519` | `resolveScopedPath(workingDir, path)` **per matched file** → Stat + EvalSymlinks again |

### 1.3 Write side

| Tool | Site | Real call |
|---|---|---|
| all four | `fs_write.go:70`, `fs_replace.go:74`, `fs_write_lines.go:75` | `CheckForbiddenWrite(joined)` → `os.Getenv(RUSH_FORBID_WRITES)` + `filepath.Abs` (`forbidden_writes.go:25-44`). `fs_delete` does **not** call it (`fs_delete.go:56-62`). |
| all four | `fs_write.go:80`, `fs_replace.go:84`, `fs_write_lines.go:85`, `fs_delete.go:68` | `permissions.Request(...)` — **not disk I/O**; see open question (b) |
| `fs_write` | preflight `fs_write.go:118` | `os.Stat(absPath)` (create-vs-overwrite) |
| `fs_write` | `fs_write.go:146` | `os.Stat(group.Path)` |
| `fs_write` | `fs_write.go:194` | `os.Stat(parent)` walking missing ancestors |
| `fs_write` | `fs_write.go:205` | `os.MkdirAll(dir, 0o755)` |
| `fs_write` | `fs_write.go:215` | `os.ReadFile(group.Path)` (old content for the diff) |
| `fs_replace` | `fs_replace.go:137,191` | `os.Stat`, `os.ReadFile` |
| `fs_write_lines` | `fs_write_lines.go:144,198` | `os.Stat`, `os.ReadFile` |
| `fs_delete` | `fs_delete.go:116,145` | `os.Stat`, **`os.Remove`** |
| write/replace/write_lines | `fs_write.go:268`, `fs_replace.go:257`, `fs_write_lines.go:348` → `commitFileChange` `edit.go:246-275` | **`fsext.AtomicWriteFile(path, data, 0o644)`** (`internal/fsext/atomic.go:23-56`: `os.CreateTemp` + `Write` + `Chmod` + `Sync` + `Close` + `os.Rename`, `os.Remove` on failure) — the fork's crash-safety patch, then `history.GetByPathAndSession/Create/CreateVersion` (**DB**) and `filetracker.RecordRead` (**session state**) |
| `fs_replace`, `fs_write_lines` | `fs_replace.go:170,188`, `fs_write_lines.go:177,195` | `filetracker.LastReadTime` / `RecordRead` — **session state**, not disk |

### 1.4 Error-shape dependencies that must survive abstraction

- `errors.Is(err, fs.ErrNotExist)` / `os.IsNotExist(err)`:
  `fs_scope.go:47`, `fs_write.go:128,148`, `fs_replace.go:140`,
  `fs_write_lines.go:147`, `fs_delete.go:118`.
- `osFailureIsFatal(err)` (`os_failure_windows.go` / `os_failure_unix.go`)
  is applied to Stat/Read/Mkdir errors at `fs_write.go:206`,
  `fs_replace.go:157,193`, `fs_write_lines.go:164,200`. It unwraps to a
  native `syscall.Errno`; a provider error that carries none returns
  `false` ⇒ recoverable level-1 tool response. That is the **safe**
  direction and needs no change, but must be documented.
- `contentTooLargeError` from `readTextFile` (`view.go:381`) is matched
  with `errors.As` at `fs_read.go:159` — it is produced *above* the
  provider boundary and is unaffected.

---

## 2. Where the abstraction seam goes (and where it does not)

Three seams were considered:

1. **Fine-grained POSIX surface only** (`Stat/Open/ReadDir/Walk/…`) and
   reimplement `fsext.ListDirectory`, `globFiles` and both grep engines
   on top of it. Rejected: it forces a rewrite of `internal/fsext` and of
   `grep.go`/`rg.go`, which the legacy `grep`/`glob`/`ls` tools share —
   explicitly out of scope for #859 ("legacy tools still hit the real
   disk"), and a far larger blast radius than the four tasks describe.
2. **Coarse task surface only** (`Read/Write/List/Find/Search/Delete`).
   Rejected: `resolveScopedPath` genuinely needs `Stat` + `EvalSymlinks`
   as primitives, and `fs_write` genuinely needs `MkdirAll` and an
   ancestor `Stat` walk to keep the T6 scope-escape check honest
   (`fs_write.go:188-202`).
3. **Hybrid (chosen)**: seven fine-grained primitives for everything the
   `fs_*` files call directly, plus three composite operations
   (`List`/`Find`/`Search`) for the three places the tools delegate into
   `fsext`/ripgrep. The composites' **default implementations are exactly
   today's code**, so nothing moves and behaviour is byte-for-byte.

The composite `Search` is also the answer to open question (c): the
ripgrep subprocess is an implementation detail of `OSDisk`, invisible to
the interface. A custom provider implements `Search` itself and **no `rg`
process is ever spawned** for a provider-backed call.

---

## 3. Task #856 — the interface and the OS-backed default

### 3.1 Placement

**Package `internal/agent/tools`, new files `fs_provider.go` (interface +
value types) and `fs_provider_os.go` (default implementation).**

This is a deliberate deviation from the FolderScope precedent, where the
policy type lives in the leaf package `internal/permission`
(`folderscope.go:48-134`) and `sdk` aliases it (`sdk/sdk.go:90-116`).
Justification:

- `OSDisk`'s `Find` and `Search` must call `globFiles` (`glob.go:89`) and
  the two grep engines (`fs_grep.go:222-234`), which are **unexported
  members of package `tools`** and are also used by the legacy `glob`/
  `grep` tools. A leaf package would require moving `rg.go`, half of
  `glob.go` and half of `grep.go` out of `tools` — violating #856's
  explicit constraint "no existing file touched yet" and dragging the
  legacy tools into the refactor.
- `sdk` already aliases internal types from a non-leaf package
  (`CredentialSet = agent.CredentialSet`, `sdk/sdk.go:80-86`), so the
  precedent for aliasing out of a "thick" package exists.
- Import graph stays acyclic: `agent` → `tools` already
  (`coordinator_tools.go:20`); `tools` imports nothing from `agent`.
  `app` → `tools` is new but acyclic; `sdk` → `tools` likewise.

*Flagged for the orchestrator (§8, item 6): if you prefer the leaf-package
shape, the cost is one extra task to move `rg.go` + the shared grep/glob
engine code into `internal/fsprovider` and have `tools` import it back.*

### 3.2 The interface

```go
// DiskProvider is the filesystem ONE fs_* tool call runs against. The
// zero interface value (nil) is not a provider: every constructor
// normalises nil to OSDisk(), the real filesystem, so an unset provider
// is exactly today's behaviour.
//
// Every method takes a context so a network- or DB-backed provider can
// honour the turn's cancellation and the fs_grep per-item timeout
// (fs_grep.go:124). Implementations that cannot block may ignore it.
//
// Paths handed to every method are ABSOLUTE and already resolved by
// resolveScopedPath (symlink-resolved existing prefix + literal tail),
// and have already passed the call's permission.FolderScope check. A
// provider is NOT a security boundary and must not be treated as one:
// it is the caller's own in-process Go code (see the trust-boundary note
// in sdk/README.md).
type DiskProvider interface {
	// Stat returns the metadata of name. A missing path MUST be reported
	// as an error satisfying errors.Is(err, fs.ErrNotExist); five call
	// sites branch on exactly that (fs_scope.go:47, fs_write.go:128,
	// fs_replace.go:140, fs_write_lines.go:147, fs_delete.go:118) and a
	// provider that reports "missing" any other way turns a create into
	// a hard failure and a delete into "cannot access".
	//
	// Only IsDir, ModTime and Mode().IsRegular() are consumed by the
	// fs_* family; Size and Sys may be zero values.
	Stat(ctx context.Context, name string) (fs.FileInfo, error)

	// EvalSymlinks resolves every symbolic link in name, which the
	// caller guarantees exists. A provider with no link concept returns
	// filepath.Clean(name), nil. An error fails path resolution CLOSED
	// (fs_scope.go:57-62) — never return a best-effort path.
	EvalSymlinks(ctx context.Context, name string) (string, error)

	// Open returns a streaming reader over name. fs_read reads at most
	// the requested line window through it, so a provider must not be
	// tempted to materialise a huge file: a ranged read of a very large
	// file is cheap today and must stay cheap.
	Open(ctx context.Context, name string) (io.ReadCloser, error)

	// ReadFile returns the whole content of name. Used for the
	// old-content snapshot of fs_write/fs_replace/fs_write_lines.
	ReadFile(ctx context.Context, name string) ([]byte, error)

	// MkdirAll creates dir and every missing parent. The caller has
	// already scope-checked every ancestor it is about to create
	// (fs_write.go:188-202), so this method must not widen that: create
	// dir and its parents, nothing else.
	MkdirAll(ctx context.Context, dir string, perm fs.FileMode) error

	// WriteFile replaces name's entire content. It MUST be
	// crash-atomic with respect to readers — either the old content or
	// the whole new content, never a truncated prefix. OSDisk implements
	// it with fsext.AtomicWriteFile (write-temp + fsync + rename), the
	// fork's kill-9 patch; an in-memory provider gets this for free.
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error

	// Remove deletes the single regular file name. The caller has
	// already refused directories and irregular files
	// (fs_delete.go:131-144); a provider must not recurse.
	Remove(ctx context.Context, name string) error

	// List enumerates a directory subtree. See ListRequest/ListResult
	// for the exact spelling contract the tree renderer depends on.
	List(ctx context.Context, req ListRequest) (ListResult, error)

	// Find matches file NAMES by glob under a root.
	Find(ctx context.Context, req FindRequest) (FindResult, error)

	// Search matches file CONTENT by regexp under a root and returns hit
	// lines plus their context lines. OSDisk implements it with ripgrep
	// when available and an in-process walk otherwise; a custom provider
	// implements it directly and NO rg subprocess is ever spawned for
	// that call.
	Search(ctx context.Context, req SearchRequest) (SearchResult, error)
}
```

### 3.3 Value types

```go
// ListRequest mirrors fsext.ListDirectory's parameters one-to-one
// (internal/fsext/ls.go:274).
type ListRequest struct {
	// Dir is the absolute, resolved directory to list.
	Dir string
	// IgnorePatterns are filepath.Match patterns tested against each
	// entry's BASE name, as fsext's shouldIgnore does (ls.go:235-241).
	IgnorePatterns []string
	// Depth is the maximum traversal depth; 0 means unlimited, matching
	// fastwalk.Config.MaxDepth.
	Depth int
	// Limit caps the number of entries; 0 means unlimited.
	Limit int
}

// ListResult is one directory listing.
type ListResult struct {
	// Entries are absolute paths of every file and directory found,
	// EXCLUDING Dir itself. Two spelling rules the renderer depends on:
	//
	//   1. every DIRECTORY entry ends with a native path separator —
	//      createFileTree decides file-vs-directory from that suffix
	//      (ls.go:206);
	//   2. every entry must start with the Dir string as spelled in the
	//      request, because createFileTree does a literal
	//      strings.TrimPrefix(path, rootPath) (ls.go:176).
	//
	// Forward-slashed entries are accepted: fs_list runs each through
	// filepath.FromSlash before the scope check and the render
	// (fs_list.go:136), which is a no-op on already-native paths.
	Entries []string
	// Truncated reports that Limit cut the listing short.
	Truncated bool
}

// FindRequest mirrors globFiles' parameters (glob.go:89).
type FindRequest struct {
	// Pattern is a doublestar-style glob ("**/*.go", "cmd/*.md").
	Pattern string
	// Dir is the absolute, resolved search root.
	Dir string
	// Limit caps the number of results; 0 means unlimited.
	Limit int
}

// FindResult is one name search.
type FindResult struct {
	// Paths are absolute matches, shortest-first (runRipgrep's stable
	// sort, glob.go:124-126) — fs_find re-sorts nothing and renders them
	// in order.
	Paths []string
	// Truncated reports that Limit cut the result short.
	Truncated bool
}

// SearchRequest is one content search over a subtree.
type SearchRequest struct {
	// Pattern is an RE2 source string. The caller has ALREADY escaped it
	// when the model asked for literal text (fs_grep.go:120-123), so an
	// implementation must treat it as a regexp unconditionally.
	Pattern string
	// Dir is the absolute, resolved search root.
	Dir string
	// Include, when non-empty, is a shell-style glob ("*.js",
	// "*.{ts,tsx}") a file path must match.
	Include string
	// ContextLines is the radius: lines emitted on EACH side of a hit.
	// 0 means hit lines only. The caller has already range-checked it
	// against FSBatchMaxContextLines (fs_grep.go:108-111).
	ContextLines int
	// MaxLines is an advisory cap on the number of DISTINCT (Path, Line)
	// pairs to produce, counting hits and context lines alike. It is a
	// HINT: the caller enforces the real budget itself
	// (fsGrepBudget/fsGrepFileHits.add), so exceeding it wastes work but
	// changes no output. 0 means no hint.
	MaxLines int
}

// SearchLine is one produced line.
type SearchLine struct {
	// Path is the absolute path of the file the line belongs to. It is
	// re-resolved and re-scope-checked by fs_grep before rendering
	// (fs_grep.go:503-538) — a root grant does not vouch for it.
	Path string
	// Line is the 1-based line number.
	Line int
	// Text is the line's text with its trailing newline removed. Leading
	// and trailing whitespace handling is engine-defined and preserved
	// verbatim by OSDisk (see §3.5, note 3).
	Text string
	// Hit is true when the line itself matched Pattern; false marks a
	// context line.
	Hit bool
}

// SearchResult is one content search's output.
type SearchResult struct {
	// Lines may arrive in ANY order and a (Path, Line) pair MAY repeat
	// (ripgrep emits a line once per role: context of one hit, match of
	// the next). The caller deduplicates and makes Hit sticky
	// (fsGrepFileHits.add, fs_grep.go:174-195), so a provider need not.
	Lines []SearchLine
}
```

### 3.4 Contract notes the implementer must honour

(a) **Not a security boundary.** The `FolderScope` check happens *above*
the provider (`fs_batch.go:291`, plus the per-result re-checks at
`fs_list.go:139`, `fs_find.go:116`, `fs_grep.go:526`). A provider that
returns paths outside the scope has them dropped, not honoured — but a
provider that *writes* somewhere else is entirely trusted to behave.

(b) **`Stat` not-exist reporting** is a hard obligation (§1.4). The
package should export
`var ErrNotExist = fs.ErrNotExist` as documentation-by-symbol.

(c) **`WriteFile` atomicity** is an obligation, not a suggestion — the
fork's `AtomicWriteFile` note (`internal/fsext/atomic.go:9-22`) explains
the failure it prevents.

(d) **`osFailureIsFatal`** returns `false` for any error not wrapping a
native errno, so a provider error is always classified recoverable
(level-1 tool response). Document; do not change.

(e) **`filepath.Abs` stays outside the interface.** `resolveScopedPath`
(`fs_scope.go:28`) only consults the process cwd when
`SmartJoin(workingDir, raw)` is relative, which requires a relative
`workingDir`. Every production caller passes `c.cfg.WorkingDir()` /
`app.config.WorkingDir()`, which is absolute. Adding an `Abs` method
would put a cwd concept into a virtual filesystem for no gain. Recorded
as an invariant, and asserted in a test (§3.7).

### 3.5 `OSDisk()` — the default implementation

```go
// OSDisk returns the real-filesystem DiskProvider: the exact os/filepath
// and search calls the fs_* family made before the provider seam
// existed, with no behavioural difference whatsoever.
func OSDisk() DiskProvider
```

Returns a stateless value (an empty struct), so it can be a package-level
singleton and needs no lifecycle.

| Method | Implementation (byte-for-byte today's code) |
|---|---|
| `Stat` | `os.Stat(name)` |
| `EvalSymlinks` | `filepath.EvalSymlinks(name)` |
| `Open` | `os.Open(name)` |
| `ReadFile` | `os.ReadFile(name)` |
| `MkdirAll` | `os.MkdirAll(dir, perm)` |
| `WriteFile` | `fsext.AtomicWriteFile(name, data, perm)` |
| `Remove` | `os.Remove(name)` |
| `List` | `fsext.ListDirectory(req.Dir, req.IgnorePatterns, req.Depth, req.Limit)` |
| `Find` | `globFiles(ctx, req.Pattern, req.Dir, req.Limit)` — keeps the rg-then-doublestar dispatch inside |
| `Search` | today's `fsGrepSearchContext(ctx, pattern, dir, include, contextLines, files, &budget)` into a local `map[string]*fsGrepFileHits` + a local budget of `req.MaxLines`, then flattened into `[]SearchLine` |

Three notes on `Search`:

1. **The engine dispatch and the `clear(files)` between engines
   (`fs_grep.go:232`) stay inside `OSDisk`.** That is why `Search`
   returns a materialised slice rather than streaming through a
   callback: a stream would need a "discard everything I already gave
   you" signal for the rg→fallback handover. Materialising is cheap —
   `FSBatchMaxGrepMatchesPerItem` is 100 (`fs_batch.go:86`).
2. **The truncation flag is computed by the tool, not the provider.**
   Today `budget.spent()` is exactly "100 distinct lines were taken"
   (`fs_grep.go:206-214`), so `fs_grep` keeps computing it from its own
   budget and `SearchResult` carries no `Truncated` field. This keeps
   the existing "(...output truncated at 100 rendered lines per item...)"
   message identical.
3. **Text normalisation is preserved verbatim, including its existing
   inconsistency.** The rg parser stores `strings.TrimSpace(...)`
   (`fs_grep.go:315`) while the fallback stores
   `strings.TrimSuffix(line, "\r")` plus `fallbackTruncateSuffix`
   (`fs_grep.go:425-430`) — leading whitespace survives on one path and
   not the other. `OSDisk.Search` must reproduce both exactly; a
   "cleanup" here would change rendered output.

`maxGrepContentWidth` truncation stays in `fsGrepFileHits.add`
(`fs_grep.go:181-183`) — above the provider — so every provider gets it.

### 3.6 How this mirrors / deviates from the FolderScope precedent

| FolderScope (T1) | DiskProvider (#856) | Why |
|---|---|---|
| Data spec compiled once into an immutable matcher (`BuildFolderScope`, `folderscope.go:149`) | A caller-supplied **behaviour**, no compile step | There is nothing to validate: an interface value either satisfies the interface or does not compile |
| Zero value denies everything (`folderscope.go:122-134`) | **nil means "real disk"**, not "deny everything" | Deliberate inversion, and the riskiest one. A "deny everything" default would break every existing caller of the 8 constructors; T8's own zero-scope pattern works because `applyCallFolderScope` separately decides *which* tools exist. Consequence: **forgetting to thread the provider silently falls back to the real disk** — which is exactly why §7's durable refusal is mandatory, and why #857/#858 pin the fall-back in tests rather than trusting it |
| Lives in leaf package `internal/permission` | Lives in `internal/agent/tools` | §3.1 |
| Serializable; T12 persists the spec (`session_runqueue.go` `FolderScopeSpec`) | **Not serializable at all** | §7 |
| Malformed spec is a hard error at `ExecuteRun` (`app_run.go:646-649`) | No malformed state exists; the analogous hard error is the "provider without scope" guard (§8 item 1) | — |

### 3.7 Tests for #856 — `internal/agent/tools/fs_provider_test.go`

Style matches the package: `t.Parallel()`, `t.TempDir()`,
`require`-based, one behaviour per test, underscore-qualified names for
helper-level units (`fs_scope_test.go:18` `TestResolveScopedPath_…`).

- `TestOSDisk_StatMissingPathIsFsErrNotExist` — the §1.4 obligation.
- `TestOSDisk_StatReportsDirRegularAndModTime` — the three `FileInfo`
  fields the tools actually read.
- `TestOSDisk_EvalSymlinksResolvesThroughLink` — mirror of
  `fs_scope_test.go:18`'s fixture (`os.Symlink` with the existing
  Windows-skip guard that file already uses).
- `TestOSDisk_OpenStreamsWithoutReadingWholeFile` — open a large file,
  read one line, assert the ReadCloser closes cleanly; guards the "do not
  turn `Open` into `ReadFile`" decision.
- `TestOSDisk_WriteFileIsAtomicAndLeavesNoTempFile` — write, then assert
  the directory contains exactly the target (no `*.tmp` residue), and on
  non-Windows assert the mode.
- `TestOSDisk_WriteFileFailureLeavesOriginalIntact` — target's parent
  made read-only (skip on Windows, as `forbidden_writes_test.go` does for
  platform-specific cases).
- `TestOSDisk_MkdirAllThenRemove`.
- `TestOSDisk_ListMatchesFsextListDirectory` — golden equivalence: build a
  fixture tree with a `.gitignore`, call `OSDisk().List` and
  `fsext.ListDirectory` with the same arguments, `require.Equal` on both
  returns. This is the "byte-for-byte" proof #856 asks for.
- `TestOSDisk_ListEntriesKeepTrailingSeparatorOnDirectories` — pins the
  `createFileTree` contract in §3.3.
- `TestOSDisk_FindMatchesGlobFiles` — same golden-equivalence shape
  against `globFiles`.
- `TestOSDisk_SearchReproducesFallbackWindows` — under `go test`,
  `getRg()` is `""` (`rg.go:19-21`), so this exercises engine B; assert
  the produced `SearchLine` set equals what `fsGrepSearchContext` fills
  today for the same fixture (hits at 5/8/20 with radius 2, the
  `fs_grep_test.go:48` fixture).
- `TestOSDisk_SearchWithRealRipgrepAgreesWithFallback` — follows
  `fs_grep_test.go`'s existing precedent of resolving a real `rg` via
  `exec.LookPath` to bypass the testing guard; `t.Skip` when absent.
- `TestOSDisk_SearchHonoursMaxLinesHint`.
- `TestDiskOrOS_NilIsTheRealDisk` — `diskOrOS(nil)` returns the same
  value as `OSDisk()`.

---

## 4. Task #857 — wiring the provider through `fs_scope.go` and the 8 tools

### 4.1 Constructor signatures — a **trailing** parameter, everywhere

T8's implementation note is the lesson to apply here: the eight real
constructors turned out non-uniform (`NewFSGrepTool` takes
`(workingDir, scope)` while the other seven take `scope` first —
`coordinator_tools.go:563-565` documents this in a comment). Inserting a
parameter positionally would therefore be inconsistent by construction.
**Every constructor gains `disk DiskProvider` as the LAST parameter**, so
the reversed-order outlier needs no special case:

```go
func NewFSListTool(scope permission.FolderScope, workingDir string, lsConfig config.ToolLs, disk DiskProvider) fantasy.AgentTool
func NewFSFindTool(scope permission.FolderScope, workingDir string, disk DiskProvider) fantasy.AgentTool
func NewFSReadTool(scope permission.FolderScope, workingDir string, disk DiskProvider) fantasy.AgentTool
func NewFSGrepTool(workingDir string, scope permission.FolderScope, disk DiskProvider) fantasy.AgentTool
func NewFSWriteTool(scope permission.FolderScope, permissions permission.Service, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool
func NewFSReplaceTool(scope permission.FolderScope, permissions permission.Service, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool
func NewFSWriteLinesTool(scope permission.FolderScope, permissions permission.Service, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool
func NewFSDeleteTool(scope permission.FolderScope, permissions permission.Service, workingDir string, disk DiskProvider) fantasy.AgentTool
```

Each constructor body starts with `disk = diskOrOS(disk)` **once**, so the
returned closure captures a non-nil value and no per-call nil check is
needed anywhere below.

### 4.2 Shared plumbing changes

**`fs_scope.go`** — `resolveScopedPath` gains a context and a provider:

```go
func resolveScopedPath(ctx context.Context, disk DiskProvider, workingDir, raw string) (abs string, err error)
```
`os.Stat` (:38) → `disk.Stat(ctx, prefix)`; `filepath.EvalSymlinks` (:57)
→ `disk.EvalSymlinks(ctx, prefix)`. Everything else is unchanged pure
path math. Callers: `fs_batch.go:270`, `fs_grep.go:519`, and every test
helper that builds a scope (`fs_batch_test.go:71`, `fs_write_test.go:28`,
`fs_grep_test.go:35-38`) — those pass `OSDisk()`.

**`fs_batch.go`** — two changes:

```go
type FSBatch[I any] struct {
	// …existing fields…
	// Disk is the filesystem this call's path resolution and execution
	// run against. nil is the real disk.
	Disk DiskProvider
}

// FSPreflightFunc gains a context so a preflight can stat through the
// provider (fs_write is the only one that does today).
type FSPreflightFunc[I any] func(ctx context.Context, item I, index int, absPath string) (permission.FileOp, error)
```
`RunFSBatch` normalises `batch.Disk` once at the top and passes it to
`resolveScopedPath` (:270) and to `batch.Preflight` (:281). The preflight
signature change touches all eight preflight funcs (seven of which just
add `_ context.Context`) plus `fs_batch_test.go:34`'s fake.

**`view.go`** — split, without touching the legacy `view` tool's body:

```go
// readTextFile keeps its exact signature for the legacy view tool.
func readTextFile(filePath string, offset, limit, maxContentSize int) (string, bool, error) {
	return readTextFileFrom(context.Background(), OSDisk(), filePath, offset, limit, maxContentSize)
}

func readTextFileFrom(ctx context.Context, disk DiskProvider, filePath string, offset, limit, maxContentSize int) (string, bool, error)
```
`os.Open` (:344) → `disk.Open(ctx, filePath)`. `view.go:269`'s call site
is unchanged.

**`edit.go`** — `editContext` gains a provider and `commitFileChange`
uses it:

```go
type editContext struct {
	ctx         context.Context
	permissions permission.Service
	files       history.Service
	filetracker filetracker.Service
	workingDir  string
	// disk is the filesystem the final write lands on. nil is the real
	// disk, which is what every legacy caller (edit, write, multiedit)
	// leaves it as.
	disk DiskProvider
}
```
`commitFileChange` (:247): `fsext.AtomicWriteFile(filePath, …)` →
`diskOrOS(edit.disk).WriteFile(edit.ctx, filePath, []byte(newContent), 0o644)`.
Nothing else in `edit.go` changes: `createNewFile`'s own
`AtomicWriteFile` (:173), `loadExistingFile`'s `os.Stat`/`os.ReadFile`
(:285, :318) belong to the legacy `edit` tool and stay on the real disk
by design (#859's documented non-goal).

### 4.3 Deviation from #857's stated file list — call it out

The task says "`fs_scope.go` and the 8 `fs_*.go` files". The real call
graph forces **two more files**:

- **`view.go`** — `fs_read`'s only actual read happens in `readTextFile`,
  which lives here and is shared with the legacy `view` tool. The split
  above is the minimum change that redirects `fs_read` without touching
  `view`'s behaviour.
- **`edit.go`** — `fs_write`/`fs_replace`/`fs_write_lines` all write
  through `commitFileChange`, which lives here and is shared with the
  legacy `edit`/`write`/`multiedit` tools. Without the `editContext.disk`
  field, three of the eight tools would still write to the real disk —
  the feature would be silently half-implemented for exactly the
  operations that matter most.

Additionally, `fs_batch.go` (shared runner) and `fs_grep.go`'s render
helper are touched, so #857's real file list is:

`fs_scope.go`, `fs_batch.go`, `fs_read.go`, `fs_list.go`, `fs_find.go`,
`fs_grep.go`, `fs_write.go`, `fs_replace.go`, `fs_write_lines.go`,
`fs_delete.go`, **`view.go`**, **`edit.go`** (12 files).

### 4.4 Per-tool call-site changes

| File | Change |
|---|---|
| `fs_read.go` | ctor captures `disk`; `fsReadExecute` becomes a closure `fsReadExecute(disk)` and stops discarding `ctx`; `fsReadOne(ctx, disk, absPath, rawPath, item)` calls `readTextFileFrom` (:156) |
| `fs_list.go` | `fsListExecute(scope, lsConfig, disk)`; `fsListOne(ctx, disk, …)`: `os.Stat` (:116) → `disk.Stat`; `fsext.ListDirectory` (:125) → `disk.List(ctx, ListRequest{Dir: absPath, IgnorePatterns: item.Ignore, Depth: cmp.Or(item.Depth, cfgDepth), Limit: maxFiles})`; the `filepath.FromSlash` + scope-filter + `createFileTree` block (:132-155) is unchanged |
| `fs_find.go` | `fsFindExecute(scope, disk)`; `fsFindOne` (:106) → `disk.Find(ctx, FindRequest{Pattern: item.Pattern, Dir: searchPath, Limit: FSFindMaxResults})`; the scope filter + `normalizeFilePaths` block is unchanged |
| `fs_grep.go` | `fsGrepRunItem(ctx, disk, workingDir, scope, rootPath, item)`: replace `fsGrepSearchContext(...)` (:129) with `disk.Search(searchCtx, SearchRequest{Pattern: pattern, Dir: rootPath, Include: item.Include, ContextLines: item.ContextLines, MaxLines: FSBatchMaxGrepMatchesPerItem})`, then feed every returned `SearchLine` through the **unchanged** `fsGrepFileHits.add(ln.Line, ln.Text, ln.Hit, &budget)`, breaking when `add` returns false. `fsGrepRender` gains `(ctx, disk, …)` for its `resolveScopedPath` (:519). `fsGrepSearchContext`/`searchWithRipgrepContext`/`searchFilesWithRegexContext`/`scanFileWithContext`/`parseRipgrepContextStream` stay in this file, now called **only** by `OSDisk.Search` |
| `fs_write.go` | `fsWritePreflight` becomes a factory `fsWritePreflight(disk) FSPreflightFunc[FSWriteItem]` (its `os.Stat` at :118); `fsWriteExecuteGroup(ctx, scope, files, filetracker, workingDir, disk, group)`: `os.Stat` (:146, :194) → `disk.Stat`; `os.MkdirAll` (:205) → `disk.MkdirAll`; `os.ReadFile` (:215) → `disk.ReadFile`; `editContext{… disk: disk}` (:261-267) |
| `fs_replace.go` | `os.Stat` (:137), `os.ReadFile` (:191) → provider; `editContext{… disk: disk}` (:250-256) |
| `fs_write_lines.go` | `os.Stat` (:144), `os.ReadFile` (:198) → provider; `editContext{… disk: disk}` (:341-347) |
| `fs_delete.go` | `fsDeleteExecuteGroup(ctx, disk, group)`: `os.Stat` (:116) → `disk.Stat`; `os.Remove` (:145) → `disk.Remove` |

`CheckForbiddenWrite` (`fs_write.go:70`, `fs_replace.go:74`,
`fs_write_lines.go:75`) stays on the real-disk env check — see open
question (b).

### 4.5 Tests for #857

New file `internal/agent/tools/fs_provider_fake_test.go` with the shared
fake, then one test per tool. The fake is the load-bearing piece:

```go
// fakeDisk is an in-memory DiskProvider for the fs_* tool tests. Every
// method records its name so a test can prove which operations the tool
// performed, and no method ever touches the real filesystem — the tests
// additionally assert the real working directory is still EMPTY
// afterwards, so a missed redirect fails loudly instead of silently
// passing against a temp dir.
type fakeDisk struct {
	mu    sync.Mutex
	files map[string]string // absolute path -> content
	dirs  map[string]bool
	calls []string          // "Stat:/abs/p", "WriteFile:/abs/p", …
	// searchLines is returned verbatim by Search.
	searchLines []SearchLine
	// listEntries / findPaths likewise.
	listEntries []string
	findPaths   []string
}
```

Required tests (names follow the existing `TestFS<Tool><Behaviour>` /
`TestResolveScopedPath_<Behaviour>` conventions):

- `TestResolveScopedPath_UsesInjectedDiskProvider` — a path that does not
  exist on the real disk but does in the fake resolves successfully, and
  a fake symlink is followed; proves `fs_scope.go` is fully redirected.
- `TestFSReadUsesInjectedDiskProvider` — content comes from the fake; the
  real temp dir stays empty.
- `TestFSListUsesInjectedDiskProvider` — assert the tool called
  `disk.List` (not `fsext.ListDirectory`) and that the fake's
  trailing-separator directory entries render as directories.
- `TestFSFindUsesInjectedDiskProvider`.
- `TestFSGrepUsesInjectedDiskProvider` — fake `Search` returns two hits
  and their context, and the rendered `<match>` block matches the
  existing golden shape.
- `TestFSGrepNeverRunsRipgrepWithInjectedProvider` — the fake records
  exactly one `Search` call and the tool's output is derived only from
  its lines (an rg spawn would add lines the fake never returned).
- `TestFSWriteUsesInjectedDiskProvider` — plus
  `require.Empty(t, realDirEntries)` after the call.
- `TestFSReplaceUsesInjectedDiskProvider`,
  `TestFSWriteLinesUsesInjectedDiskProvider`,
  `TestFSDeleteUsesInjectedDiskProvider`.
- `TestFSWriteHistoryAndFileTrackerStillRecordWithInjectedProvider` —
  pins the answer to open question (b) whichever way it is decided.
- `TestFSToolsNilDiskProviderStillUseTheRealDisk` — a table over all
  eight constructors with `nil`; the existing per-tool tests (which
  simply gain a `nil` argument) are the broader proof.

Existing tests change **only** by gaining the new trailing `nil`
argument — a mechanical edit that is itself the "zero behaviour change"
evidence #857 asks for.

---

## 5. Task #858 — `CallOptions` + coordinator wiring

### 5.1 `internal/agent/call_options.go`

```go
type CallOptions struct {
	// …existing fields…

	// DiskProvider redirects THIS call's fs_* filesystem I/O to a
	// caller-supplied implementation instead of the real disk: every
	// stat, symlink resolution, read, write, delete, directory listing,
	// name search and content search the eight fs_* tools perform goes
	// through it, including the path resolution the folder-scope check
	// runs on. nil (the default) is the real filesystem, so every
	// existing caller is unchanged.
	//
	// Deliberately NOT symmetrical with FolderScope above: FolderScope
	// is data that BuildFolderScope compiles and T12 persists onto a
	// durable run-queue row; a DiskProvider is arbitrary in-process Go
	// code that cannot be serialized at all. A call carrying one must
	// therefore never reach the durable queue — see
	// refuseDurableDiskProviderCall.
	//
	// It does NOT affect the legacy single-target file tools
	// (view/glob/grep/ls/write/edit/multiedit), bash, download,
	// git_read, agentic_fetch or MCP tools, all of which keep hitting
	// the real disk. It is the caller's own in-process code, not a
	// sandbox.
	DiskProvider tools.DiskProvider
}
```

`call_options.go` gains an `internal/agent/tools` import. Acyclic:
`tools` imports nothing from `agent` (verified — `coordinator_tools.go:20`
is the existing `agent`→`tools` edge).

`WithCallOptions` (`call_options.go:118-124`) needs **no** change: it
mirrors only the prompt-relevant folder-scope bit, and §6.4 concludes the
prompt does not change.

### 5.2 `internal/agent/coordinator_tools.go` — `buildTools`

The scope extraction at `:463-466` becomes a joint extraction:

```go
	var scope permission.FolderScope
	var disk tools.DiskProvider
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		if callOpts.FolderScope != nil {
			scope = *callOpts.FolderScope
		}
		disk = callOpts.DiskProvider
	}
```

and the eight constructions at `:566-573` each gain `disk` as their last
argument. The existing comment block at `:562-565` (which already warns
about `NewFSGrepTool`'s reversed argument order) gets one more sentence
noting that `disk` is always last precisely because of that asymmetry.

`applyCallFolderScope` (`:355-394`) is **not** changed: the provider does
not decide which tools exist, only where their I/O lands. That is a
deliberate deviation from T8 — and it is exactly the hole open question
(1) in §8 is about.

**No change is needed to `withoutCallOptions`** (`coordinator_models.go:517-524`):
it already replaces the whole `*CallOptions` with a typed nil, so
`UpdateModels`' global publish can never carry a caller's provider. The
#858 test pins that rather than adding code.

### 5.3 Tests for #858 — `internal/agent/coordinator_disk_provider_test.go`

Shaped after `coordinator_tool_pinning_test.go` (R3-1): reuse
`newToolPinningCoordinator`, `toolPublishRecorder`, `pinnedToolNames`.

- `TestResolveSessionModels_PinsPerCallDiskProvider` — two
  `resolveSessionModels` calls on the same session, one context carrying
  a `fakeDisk`, one not. Extract `fs_read` from each pinned slice, `Run`
  both, and assert the first read the fake's content while the second
  read the real temp file; then re-resolve the provider-carrying context
  **after** the plain one and assert it still sees the fake (no bleed).
  Finish with the R3-1 key assertion: `rec.snapshot()` shows zero
  `SetTools` and zero `SetModels`.
- `TestUpdateModels_NeverPublishesCallerDiskProvider` — mirror of
  `TestUpdateModels_NeverPublishesPerCallToolFilter`
  (`coordinator_tool_pinning_test.go:255-280`): call `UpdateModels` with a
  provider-carrying context, take the single published slice, run its
  `fs_read` and assert it hit the **real** disk.
- `TestBuildTools_SubAgentInheritsCallerDiskProvider` — `buildTools(ctx,
  cfg, taskCfg, true)`; the sub-agent's `fs_read` must also read from the
  fake, matching how the folder scope already reaches sub-agent builds.
- `TestBuildTools_NilDiskProviderKeepsRealDisk`.

---

## 6. Task #859 — SDK surface

### 6.1 `internal/app/app_run.go`

```go
type RunOverrides struct {
	// …existing fields…

	// DiskProvider, when non-nil, replaces the real filesystem for
	// THIS run's fs_* tools (fs_read, fs_list, fs_find, fs_grep,
	// fs_write, fs_replace, fs_write_lines, fs_delete) and for the path
	// resolution their folder-scope checks run on. Per-call only and
	// NEVER persisted on the session, exactly like FolderScopes above.
	// A run carrying one is refused if it would be durably queued (see
	// §7) because a Go value cannot be serialized onto a run-queue row.
	DiskProvider tools.DiskProvider
}
```

Wired in `ExecuteRun`'s `callOpts` literal (`app_run.go:685-713`), one
line next to `FolderScope`:

```go
		DiskProvider: overrides.DiskProvider,
```

Plus the guard recommended in §8 item 1, placed immediately after the
folder-scope compile block (`app_run.go:636-665`), so it fails before any
session work or provider traffic — the same placement and the same
"hard error, not a warning" shape as `invalid folder scopes`
(`app_run.go:646-649`).

**No `fsToolsForScope` analogue is needed.** T10's mandatory footgun fix
(`app_run.go:659-664`) existed because `RestrictedRun` + an empty
`AllowTools` table denies every plain tool at the permission gate. A disk
provider changes neither the tool set nor the permission gate, so the
restricted-run interaction is unchanged. This is a genuine
non-requirement, verified by reading `fsToolsForScope`
(`app_run_gates.go:125-155`) — recorded here because #859's task text
asks the question explicitly.

### 6.2 `sdk/sdk.go`

Alongside the folder-scope aliases (`sdk/sdk.go:90-116`):

```go
// Disk-provider aliases (RunOverrides.DiskProvider): aliases onto the
// canonical types in internal/agent/tools that the fs_* tools consume,
// so a host's implementation is directly assignable with zero
// conversion code — the same aliasing pattern as FolderScope above.
type (
	// DiskProvider is the filesystem one run's fs_* tools operate on.
	// See tools.DiskProvider for the full method contract.
	DiskProvider = tools.DiskProvider

	DiskListRequest   = tools.ListRequest
	DiskListResult    = tools.ListResult
	DiskFindRequest   = tools.FindRequest
	DiskFindResult    = tools.FindResult
	DiskSearchRequest = tools.SearchRequest
	DiskSearchResult  = tools.SearchResult
	DiskSearchLine    = tools.SearchLine
)

// OSDiskProvider returns the real-filesystem provider — the default a
// run gets when RunOverrides.DiskProvider is nil. Exposed so a host can
// wrap or decorate it (audit logging, read-only enforcement) instead of
// reimplementing ten methods.
func OSDiskProvider() DiskProvider { return tools.OSDisk() }
```

`OSDiskProvider` is a deliberate addition beyond the task text: without
it, a host that wants "real disk, but log every write" has to reimplement
ripgrep dispatch and gitignore-aware walking. Cheap, and it makes the
default implementation's behaviour reusable rather than trapped.

### 6.3 `sdk/README.md`

New section after "Scoped filesystem access" (`sdk/README.md:184-232`),
matching its voice and its "boundaries to know" list:

```
## Substituting the filesystem: `RunOverrides.DiskProvider`
```

covering, per #859's requirements:

- **What it replaces**: every real disk operation of the eight `fs_*`
  tools — stat, symlink resolution, streaming read, whole-file read,
  `MkdirAll`, atomic write, remove, directory listing, name glob and
  content search — plus the path resolution the folder-scope check runs
  on, so a virtual path is scope-checked exactly like a real one.
- **What it does NOT affect** (the deliberate non-retrofit): `view`,
  `write`, `edit`, `multiedit`, `glob`, `grep`, `ls`, `download`,
  `git_read`, `agentic_fetch`, MCP tools, and `bash`/`run_command` all
  keep hitting the real filesystem. A scoped call already strips the
  first seven and the escape hatches
  (`coordinator_tools.go:296-312`), which is why §8 item 1 recommends
  requiring a scope.
- **The trust boundary**: the provider is the host's own in-process Go
  code running in the same address space with the same OS privileges as
  `rush`. It is a *redirection* mechanism, not a sandbox: it does not
  confine `bash`, does not confine anything the provider itself chooses
  to do, and is not a defence against a hostile model that has other
  tools. The `FolderScope` ACL is still what limits which paths the
  model may name.
- **Per-call only, never persisted** — same wording as the FolderScopes
  bullet at `sdk/README.md:219-222`.
- **Never survives a durable restart** — §7's refusal, stated as a
  behaviour, not a caveat.

While editing this file, also fix the now-stale bullet at
`sdk/README.md:227-232` ("Durable-restart gap (temporary) … a scoped SDK
turn … currently restarts **unscoped**"), which the uncommitted T12 work
(`coordinator_interrupt.go:509-532`, `:604-626`) has already closed.
Flagged rather than assumed: confirm T12 is committed before rewriting
it.

### 6.4 Does the model prompt change? — **No.** (decided, not skipped)

`prompt.PromptDat.FolderScoped` (`prompt/prompt.go:68-79`) drives the
`<scoped_filesystem>` block (`templates/coder.md.tpl:22-37`). That block
tells the model three things: which tools are absent, that every call
takes an `items` array, and that line ranges are 1-based. A disk provider
changes **none** of them — same tool names, same JSON schemas, same
per-item denial semantics, same error text. Telling the model "your
filesystem is virtual" would be information it cannot act on, and would
risk it second-guessing correct results.

**One exception, and it is why the question is not vacuous**: if command
tools survive into the call (`FolderScope.KeepsCommandTools()`,
`folderscope.go:283`, set at `app_run.go:644` when `RestrictedRun` is on
with bash patterns), then `bash` sees the **real** disk while `fs_read`
sees the virtual one — the model's assumption that "the file I just wrote
is the file my test command reads" becomes false, silently. §8 item 2
proposes refusing that combination outright (T9's shape) so the prompt
genuinely never needs to change; the alternative is a second prompt flag
and one extra sentence in the template.

### 6.5 Tests for #859

`sdk/sdk_disk_provider_test.go`, shaped after `sdk_folder_scope_test.go`
(scripted openai-compat SSE server, `toolCallChunkNamed`,
`newFSRoundServer` — `sdk_folder_scope_test.go:44-100`):

- `TestSDKRunWithDiskProviderRoundTripsWithoutTouchingRealDisk` — the
  headline test #859 asks for: round 1 the model calls `fs_write`, round
  2 it calls `fs_read` on the same path and the tool result carries the
  content back, round 3 it stops. Assertions: the content came back
  through `fs_read`; the fake holds the file; **and the real working
  directory is still empty** (`os.ReadDir` length 0).
- `TestSDKRunWithoutDiskProviderWritesToTheRealDisk` — the negative
  control, same script, no override, file exists on disk.
- `TestSDKRunDiskProviderWithoutFolderScopesFailsBeforeProviderTraffic` —
  mirrors `TestSDKRunInvalidFolderScopeEntryFailsBeforeProviderTraffic`
  (`sdk_folder_scope_test.go:382`), asserting the scripted server saw
  **zero** requests. (Only if §8 item 1 is adopted.)
- `TestSDKRunDiskProviderAndRealDiskRunConcurrentSessionsIsolated` —
  mirrors `TestSDKRunScopedAndUnscopedConcurrentSessionsIsolated`
  (`sdk_folder_scope_test.go:187`): two sessions in flight, one with a
  provider and one without; neither leaks into the other.
- `internal/app/app_run_durable_disk_provider_test.go`:
  `TestExecuteRunOrphanedDiskProviderCallIsNeverEnqueued` — modelled on
  the untracked `app_run_durable_restart_folder_scope_test.go:35`
  harness, but asserting the **opposite** outcome: the run-queue table
  stays empty and the run reports a refusal, instead of a rebuilt turn.

---

## 7. Durable restart — **mandatory section**

### 7.1 The mechanism, precisely

The T12 pattern (`git show 271550b1` plus the uncommitted folder-scope
half) is: keep the *uncompiled spec* on `SessionAgentCall`
(`agent.go:327-338`), mirror it into `internal/session` because
`session` cannot import `permission` (`session_runqueue.go`
`RunAllowlistSpec` / `FolderScopeSpec`; `call_data_conversion.go:72-114`),
serialize it onto the run-queue row (`ToSessionAgentCallData`
`call_data_conversion.go:132-163`), and recompile it on the way back
(`RebuildSessionAgentCall` `coordinator_interrupt.go:509-532`, then
`RunSessionAgentCall`'s toolset rebind `:604-626`).

That pattern is **structurally unavailable** for a disk provider:

- `SessionAgentCall.CallOptions` is `json:"-"` and explicitly documented
  as never persisted (`agent.go:365-372`).
- A `DiskProvider` is an interface value — typically a closure over host
  state (an open DB handle, an HTTP client, an in-memory map). There is
  no spec to mirror. Nothing in `session.SessionAgentCallData` could
  reconstruct it, in this process or another.

### 7.2 What happens today if nothing is done — the silent promotion

A turn is orphaned into the durable queue at three producers:

- `sessionAgent.restartOrphanedWithRetry` (`agent_ownership.go:394`,
  reached from `restartOrphaned` `:359`, from
  `drainOrReleaseMerged`/`drainOrReleaseFinal` when a turn's owner exits
  with queued work) — `ToSessionAgentCallData` at `:440`,
  `EnqueueRunQueueEntry` at `:459`;
- `coordinator.startDetachedRun` (`coordinator_interrupt.go:316`) —
  `ToSessionAgentCallData` at `:349`, enqueue at `:366`;
- `coordinator.handleInterruptTick`'s inject path
  (`coordinator_interrupt.go:204`) via
  `ConsumeInterruptInjectAndEnqueue`.

The pump later calls `RebuildSessionAgentCall`, which sets
`CallOptions` **only** from `data.FolderScopeSpec`
(`coordinator_interrupt.go:523-532`). The rebuilt call therefore has
`CallOptions.DiskProvider == nil`, `buildTools` normalises nil to
`OSDisk()` (§3.5), and the restarted turn replays the model's writes
**onto the operator's real disk**. Nothing in the logs would say so.

This is precisely the failure the task description names: "a scoped
in-memory-only replay landing on the REAL disk after an unannounced
restart would be a serious silent behavior change." It is worse than the
folder-scope version of the same bug (which T12 fixed), because there the
fallback was a *wider* toolset on the *same* filesystem; here the
fallback is a *different filesystem entirely*.

### 7.3 Design — refuse, at three layers

**Follow T9's "refuse outright" precedent**
(`rejectScopedCallOnCLIProvider`, `coordinator_tools.go:396-414`), not
T12's "persist and restore".

**Layer 1 — refuse to enqueue (primary).** A new predicate in package
`agent`:

```go
// ErrDiskProviderNotDurable is returned instead of enqueueing a call
// that carries a caller-supplied DiskProvider. Such a call's filesystem
// is arbitrary in-process Go code with no serializable form, so a
// durable row rebuilt from it would silently restart on the REAL disk —
// replaying the model's writes onto the operator's filesystem instead of
// the host's. Dropping the call is the fail-closed direction: the host
// is still in-process and can retry, and Run reports the refusal.
var ErrDiskProviderNotDurable = errors.New(
	"agent: a call with a caller-supplied disk provider cannot be durably queued")

// callCarriesDiskProvider reports whether call's per-call options carry a
// host-supplied filesystem.
func callCarriesDiskProvider(call SessionAgentCall) bool {
	return call.CallOptions != nil && call.CallOptions.DiskProvider != nil
}
```

Applied at every producer above, **before** `ToSessionAgentCallData`:
`restartOrphanedWithRetry` records `callErrs[i] = ErrDiskProviderNotDurable`
and `slog.Error`s it with `session_id` and `logical_call_id` (never the
prompt — the SEC-1 rule at `agent_ownership.go:461-470`);
`startDetachedRun` returns it; `handleInterruptTick` returns it and
leaves the `pending_injects` row for the operator (it must **not** be
consumed, or the message is lost with no record).

**Layer 2 — persist a refusal marker (belt and braces).** Add one boolean
to the durable mirror:

```go
// session.SessionAgentCallData
	// HostDiskProvider records that the call this row was built from
	// carried a caller-supplied filesystem (agent.CallOptions.DiskProvider).
	// The provider itself is arbitrary Go code and cannot be serialized,
	// so a row with this flag set must NEVER be executed: rebuilding it
	// would silently restart the turn on the real disk. Producers refuse
	// to enqueue such calls at all; this flag exists so that a producer
	// added later, or a row written by an older/newer binary, still fails
	// closed at the consumer instead of failing open.
	HostDiskProvider bool `json:"host_disk_provider,omitempty"`
```

`ToSessionAgentCallData` sets it from `callCarriesDiskProvider(call)`;
`RebuildSessionAgentCall` returns `ErrDiskProviderNotDurable` when it is
set, which the pump treats as a **terminal** failure (no retry loop on an
unfixable row).

**Layer 3 — the front door (optional, recommended).** `ExecuteRun` on the
SDK path sets `FailIfSessionBusy: true` (`app_run.go:712`,
`sdk_folder_scope_test.go`'s premise), so a provider-carrying SDK run
never queues behind another turn in the first place. Documenting that in
the README's boundary list turns the refusal from a surprise into a
stated contract.

### 7.4 The alternative that was rejected, and why

*"Let it fall back to the real disk and document it."* Rejected: the
whole point of injecting a provider is that the host's data must not
touch the operator's filesystem — a host may be running untrusted
tenant content, or replaying a turn against a snapshot. A fallback turns
a correctness guarantee into a timing-dependent one. The T9 precedent is
exactly this judgement call made once already: a folder-scoped call on a
CLI provider is refused rather than run with a scope that "silently means
nothing" (`coordinator_tools.go:396-402`).

*"Give the host a registry so a provider can be looked up by ID after a
restart."* Rejected for this pass, but it is the only shape that could
ever make provider-carrying calls durable, and it is a real product
feature (`RunOverrides.DiskProviderID string` + a
`Client`-scoped registry). Recorded in §8 item 5 as a non-goal, not as
an impossibility.

### 7.5 Tests for §7

- `internal/agent/agent_ownership_disk_provider_test.go`:
  `TestRestartOrphaned_RefusesToEnqueueDiskProviderCall` — a fake
  `session.Service` whose `EnqueueRunQueueEntry` fails the test if called;
  assert the returned error wraps `ErrDiskProviderNotDurable`.
- `TestStartDetachedRun_RefusesDiskProviderCall`.
- `internal/agent/rebuild_diskprovider_test.go`:
  `TestRebuildSessionAgentCall_RefusesRowMarkedHostDiskProvider` and
  `TestSessionAgentCallDataHostDiskProviderJSONRoundTrip`, mirroring the
  untracked `rebuild_folderscope_spec_test.go:24-140`.
- `internal/app/app_run_durable_disk_provider_test.go`:
  `TestExecuteRunOrphanedDiskProviderCallIsNeverEnqueued` (§6.5).
- A **revert check** is mandatory for the first and last of these: each
  must fail on the pre-fix code, because a test that passes either way
  proves nothing about a silent fallback.

---

## 8. Open questions — decisions that are NOT mine to make

Each is stated with the tradeoff, a recommendation, and what changes if
the recommendation is rejected.

**1. Should `DiskProvider` require a `FolderScope`?** *(the biggest one)*

Without a scope, an `fs_*`-capable run keeps the legacy `view`/`write`/
`edit`/`multiedit`/`glob`/`grep`/`ls` tools (they are in the default
`AllowedTools`, `config.go:787-822`), so the model can read the virtual
file with `fs_read` and then overwrite the **real** one with `write` in
the same turn. Only `applyCallFolderScope` strips them
(`coordinator_tools.go:364-366`), and it only runs when
`CallOptions.FolderScope != nil` (`:357`).

- **Recommended**: a hard error in `ExecuteRun` when `DiskProvider != nil
  && len(FolderScopes) == 0`, mirroring `invalid folder scopes`
  (`app_run.go:646-649`). Costs the host one scope entry granting every
  op on the root; buys a coherent invariant and a crisp README sentence.
- **Alternative A**: extend `applyCallFolderScope`'s strip to fire on
  "provider set" as well as "scope set". More permissive, but then the
  scoped-prompt block (`coder.md.tpl:22`) and the toolset disagree unless
  `WithCallOptions`' mirrored flag is widened too.
- **Alternative B**: document the footgun and do nothing. Not
  recommended; it is the same class of hazard T10's own footgun fix
  existed for.

**2. `DiskProvider` + `KeepCommandTools` — refuse, warn, or tell the
model?** With bash present, the model's tools straddle two filesystems
(§6.4). Recommended: **refuse** (hard error at `ExecuteRun`, T9's shape),
which also keeps the prompt unchanged. Alternative: keep it and add a
second prompt flag plus one sentence to `<scoped_filesystem>`.

**3. Should `history` / `filetracker` / `permissions.Request` /
`CheckForbiddenWrite` be redirectable too?** *(question (b) verbatim)*

My analysis, stated so it can be overruled:

- **`permissions.Request`** (`fs_write.go:80` etc.) — **keep real, not
  redirectable.** It is the operator-approval channel, not I/O. Skipping
  or redirecting it because a provider is present would be a privilege
  change dressed as plumbing, and in an interactive session it would
  remove the human from the loop.
- **`CheckForbiddenWrite`** (`fs_write.go:70`) — **keep real.** It is a
  real-disk blacklist (`RUSH_FORBID_WRITES`); against a virtual FS it is
  meaningless but can only *narrow*, and removing it would need a
  justification the feature does not have.
- **`history.Service`** (`edit.go:256-271`) — **keep real, not
  redirectable.** History is the session's audit/undo record, keyed by
  path string; a virtual path is a perfectly good key. Making it
  redirectable would mean a run whose file changes are invisible to
  `sessions diff` / the web UI's diff view, i.e. an unobservable turn.
- **`filetracker.Service`** (`fs_replace.go:170`, `fs_write_lines.go:177`)
  — **keep real, not redirectable**, same reasoning: it is per-session
  state, not I/O.

Net recommendation: **the provider redirects bytes-on-disk only.** But
this is a product decision about what "replace the filesystem" means to a
host, and item 4 below shows the current code already has a bug in this
area that the decision interacts with.

**4. Pre-existing bug found while reading (not mine to fix here).**
`fs_read` **never** calls `filetracker.RecordRead` — the only recorders in
the package are `edit.go:197,273,315`, `multiedit.go:251`, `view.go:295`,
`write.go:85,186`, and `fs_replace.go:188`/`fs_write_lines.go:195` (both
only on the *external-modification* branch). But `fs_replace`
(`:170-183`) and `fs_write_lines` (`:177-190`) **require** a non-zero
`LastReadTime` and otherwise fail with *"you must read the file before
replacing content in it. Use the fs_read tool first"*. In a folder-scoped
run the legacy `view` tool is stripped, so following that instruction is
impossible: `fs_replace`/`fs_write_lines` can only ever succeed on a file
`fs_write` already wrote in the same session (because `commitFileChange`
records at `edit.go:273`). This is independent of the disk-provider work,
but #857 edits exactly these lines, so it is the cheapest moment to fix
it. **Decision needed**: fix it inside #857 (add `filetracker.RecordRead`
to `fsReadOne`'s success path, requiring the session id and a
`filetracker.Service` parameter on `NewFSReadTool`), or file it
separately.

**5. Non-goal, recorded**: a provider *registry* keyed by a serializable
ID, which is the only way provider-carrying calls could ever survive a
durable restart (§7.4). Out of scope for #856–#859.

**6. Package placement** (§3.1): `internal/agent/tools` (recommended, no
code movement) vs a leaf `internal/fsprovider` (cleaner layering, costs a
move of `rg.go` and the shared grep/glob engine code out of `tools`).

**7. Naming**: `DiskProvider` / `OSDisk()` follow the task text. If the
orchestrator prefers `FS` / `Filesystem`, now is the moment — it is a
public SDK symbol and renaming later is a breaking change for embedders.

**8. Stale SDK README bullet** (`sdk/README.md:227-232`) claims folder
scopes do not survive a durable restart; the uncommitted T12 work closes
that. #859 touches this file — confirm T12 is committed and rewrite the
bullet, or leave it and note the sequencing.

---

## 9. Suggested ordering and non-goals

Order is the blocking chain already in the task list
(#856 → #857 → #858 → #859), with one addition: **the §7 durable-refusal
work belongs to #859**, not to a later cleanup. It is not a hardening
pass, it is the difference between "this feature is safe" and "this
feature has a silent real-disk fallback".

Explicit non-goals for these four tasks:

- Retrofitting `view`/`write`/`edit`/`multiedit`/`glob`/`grep`/`ls`,
  `download`, `git_read`, `agentic_fetch` or the MCP tools.
- Confining `bash` / `run_command` (a provider is not a sandbox).
- Any change to `internal/fsext`, `rg.go` or the legacy `grep.go` engines
  beyond what §3.1's placement decision avoids.
- Persisting a provider across processes (§8 item 5).
- Changing `permission.FolderScope` or any of its compilation, matching
  or persistence behaviour.
