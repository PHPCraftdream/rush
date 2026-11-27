package tools

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
)

// LibraryVirtualRoot is the canonical definition backing sdk.LibraryVirtualRoot
// (see that doc comment for the full history/rationale). Defined here, not
// in package sdk, because this package cannot import sdk — sdk already
// imports this package for DiskProvider/OSDisk — but a fs_* tool call
// resolving a path against the REAL disk must recognize this sentinel to
// refuse it (R6-1, P0 SDK review round 6: an OSDisk-backed tool must never
// operate against the synthetic ephemeral-library-mode namespace, no
// matter how a call reaches that far).
//
// Computed per-OS, unlike the single fixed literal a prior round (R5-6)
// used, so the value itself satisfies the REAL, platform-native
// filepath.IsAbs -- not merely filepathext.SmartIsAbs's cross-platform
// "absolute enough to skip re-rooting via filepath.Abs" notion. Before
// this fix, the one literal "/rush-library-mode-root" was genuinely
// absolute on Unix but only filepathext.SmartIsAbs (a driveless leading
// "/") on Windows -- violating DiskProvider's own documented contract
// that "every method receives an absolute path"
// (internal/agent/tools/fs_provider.go): a strict custom DiskProvider
// checking the real filepath.IsAbs could correctly reject a path the SDK
// itself produced (R6-1 finding).
//
// The Windows drive letter chosen is deliberately not one of the
// conventional ones (C, D) reserved for a real system/data volume, but
// no single-letter choice can be guaranteed collision-free against every
// possible real mapped drive on every machine. That no longer matters
// for safety: rejectRealDiskUnderLibraryVirtualRoot below refuses every
// REAL OSDisk operation resolving under this sentinel unconditionally,
// so even a machine where this exact drive letter happens to be a real,
// live volume never has it touched through this path -- the sentinel
// denies/fails closed rather than granting anything, exactly like the
// prior round's literal was already documented (but not yet enforced) to
// do.
var LibraryVirtualRoot = libraryVirtualRootForOS()

func libraryVirtualRootForOS() string {
	if runtime.GOOS == "windows" {
		return `K:\rush-library-mode-root`
	}
	return "/rush-library-mode-root"
}

// rejectRealDiskUnderLibraryVirtualRoot fails closed (returns an error,
// never a silently-substituted path) when a REAL OSDisk operation would
// resolve inside the ephemeral library-mode sentinel namespace.
// LibraryVirtualRoot is a synthesized ConfigStore.WorkingDir() placeholder
// for a session with no real working directory (sdk.ModeLibrary with no
// Options.WorkingDir) -- not an actual sandboxed root -- so the real
// filesystem has no business ever being consulted under it.
//
// This only ever fires when disk is the real filesystem (diskOrOS's nil
// fallback, or a caller explicitly handing back OSDisk()); a caller's own
// custom DiskProvider is untouched here, exactly like every other
// real-disk-specific behaviour in this package (see DiskProvider's "not a
// security boundary" doc comment) -- a custom provider's own paths, in
// whatever namespace the caller chose, are the caller's business.
//
// resolved must already be produced by resolveScopedPath's join+clean
// step (native separators, not yet symlink-walked) so the comparison
// happens BEFORE the first real disk.Stat call this function's caller is
// about to make -- the point of failing closed here is that zero real
// syscalls are issued for a sentinel-rooted path, not merely that the
// eventual result is denied.
func rejectRealDiskUnderLibraryVirtualRoot(disk DiskProvider, resolved string) error {
	if !IsOSDisk(disk) {
		return nil
	}
	if !pathIsUnderLibraryVirtualRoot(resolved, LibraryVirtualRoot) {
		return nil
	}
	return fmt.Errorf(
		"fs: refusing real-disk access under the ephemeral library-mode sentinel root %q: "+
			"this session has no real working directory (sdk.ModeLibrary with no WorkingDir) -- "+
			"supply your own DiskProvider (with a FolderScope) to enable file tools for it",
		LibraryVirtualRoot)
}

// ErrNotExist documents-by-symbol the error DiskProvider.Stat MUST satisfy
// for a missing path: errors.Is(err, ErrNotExist) must be true. It is
// exactly fs.ErrNotExist — five call sites in the fs_* family branch on
// this (fs_scope.go, fs_write.go, fs_replace.go, fs_write_lines.go,
// fs_delete.go), and a provider that reports "missing" any other way turns
// a create into a hard failure and a delete into "cannot access".
var ErrNotExist = fs.ErrNotExist

// DiskProvider is the filesystem ONE fs_* tool call runs against. The zero
// interface value (nil) is not a provider: every constructor that accepts
// a DiskProvider normalises nil to OSDisk() (see diskOrOS), the real
// filesystem, so an unset provider is exactly today's behaviour. This is a
// deliberate inversion of permission.FolderScope, whose zero value denies
// everything — there is no "deny everything" disk, only "the real one" or
// a caller-supplied substitute.
//
// Every method takes a context so a network- or DB-backed provider can
// honour the turn's cancellation and the fs_grep per-item timeout.
// Implementations that cannot block may ignore it.
//
// Paths handed to every method are ABSOLUTE and already resolved by
// resolveScopedPath (symlink-resolved existing prefix + literal tail), and
// have already passed the call's permission.FolderScope check. A provider
// is NOT a security boundary and must not be treated as one: it is the
// caller's own in-process Go code.
type DiskProvider interface {
	// Stat returns the metadata of name. A missing path MUST be reported
	// as an error satisfying errors.Is(err, fs.ErrNotExist); five call
	// sites branch on exactly that (fs_scope.go, fs_write.go,
	// fs_replace.go, fs_write_lines.go, fs_delete.go) and a provider that
	// reports "missing" any other way turns a create into a hard failure
	// and a delete into "cannot access".
	//
	// Only IsDir, ModTime and Mode().IsRegular() are consumed by the
	// fs_* family; Size and Sys may be zero values.
	Stat(ctx context.Context, name string) (fs.FileInfo, error)

	// EvalSymlinks resolves every symbolic link in name, which the caller
	// guarantees exists. A provider with no link concept returns
	// filepath.Clean(name), nil. An error fails path resolution CLOSED —
	// never return a best-effort path.
	EvalSymlinks(ctx context.Context, name string) (string, error)

	// Open returns a streaming reader over name. fs_read reads at most
	// the requested line window through it, so a provider must not
	// materialise a huge file: a ranged read of a very large file is
	// cheap today and must stay cheap.
	Open(ctx context.Context, name string) (io.ReadCloser, error)

	// ReadFile returns the whole content of name. Used for the
	// old-content snapshot of fs_write/fs_replace/fs_write_lines.
	ReadFile(ctx context.Context, name string) ([]byte, error)

	// MkdirAll creates dir and every missing parent. The caller has
	// already scope-checked every ancestor it is about to create, so
	// this method must not widen that: create dir and its parents,
	// nothing else.
	MkdirAll(ctx context.Context, dir string, perm fs.FileMode) error

	// WriteFile replaces name's entire content. It MUST be crash-atomic
	// with respect to readers — either the old content or the whole new
	// content, never a truncated prefix. OSDisk implements it with
	// fsext.AtomicWriteFile (write-temp + fsync + rename); an in-memory
	// provider gets this for free.
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error

	// Remove deletes the single regular file name. The caller has
	// already refused directories and irregular files; a provider must
	// not recurse.
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
	//
	// The result type is named DiskSearchResult, not SearchResult, to
	// avoid colliding with the unrelated web-search tool's SearchResult
	// type already declared in this package (search.go).
	Search(ctx context.Context, req SearchRequest) (DiskSearchResult, error)
}

// ListRequest mirrors fsext.ListDirectory's parameters one-to-one.
type ListRequest struct {
	// Dir is the absolute, resolved directory to list.
	Dir string
	// IgnorePatterns are filepath.Match patterns tested against each
	// entry's BASE name, as fsext's shouldIgnore does.
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
	//  1. every DIRECTORY entry ends with a native path separator —
	//     createFileTree decides file-vs-directory from that suffix
	//     (internal/fsext/ls.go:206);
	//  2. every entry must start with the Dir string as spelled in the
	//     request, because createFileTree does a literal
	//     strings.TrimPrefix(path, rootPath) (internal/fsext/ls.go:176).
	//
	// Forward-slashed entries are accepted: fs_list runs each through
	// filepath.FromSlash before the scope check and the render, which is
	// a no-op on already-native paths.
	Entries []string
	// Truncated reports that Limit cut the listing short.
	Truncated bool
}

// FindRequest mirrors globFiles' parameters.
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
	// sort) — fs_find re-sorts nothing and renders them in order.
	Paths []string
	// Truncated reports that Limit cut the result short.
	Truncated bool
}

// SearchRequest is one content search over a subtree.
type SearchRequest struct {
	// Pattern is an RE2 source string. The caller has ALREADY escaped it
	// when the model asked for literal text, so an implementation must
	// treat it as a regexp unconditionally.
	Pattern string
	// Dir is the absolute, resolved search root.
	Dir string
	// Include, when non-empty, is a shell-style glob ("*.js",
	// "*.{ts,tsx}") a file path must match.
	Include string
	// ContextLines is the radius: lines emitted on EACH side of a hit.
	// 0 means hit lines only. The caller has already range-checked it
	// against FSBatchMaxContextLines.
	ContextLines int
	// MaxLines is an advisory cap on the number of DISTINCT (Path, Line)
	// pairs to produce, counting hits and context lines alike. It is a
	// HINT: the caller enforces the real budget itself
	// (fsGrepBudget/fsGrepFileHits.add), so exceeding it wastes work but
	// changes no output. 0 means no hint (no cap applied by the
	// provider).
	MaxLines int
}

// SearchLine is one produced line.
type SearchLine struct {
	// Path is the absolute path of the file the line belongs to. It is
	// re-resolved and re-scope-checked by fs_grep before rendering — a
	// root grant does not vouch for it.
	Path string
	// Line is the 1-based line number.
	Line int
	// Text is the line's text with its trailing newline removed. Leading
	// and trailing whitespace handling is engine-defined and preserved
	// verbatim by OSDisk (see OSDisk's Search doc comment).
	Text string
	// Hit is true when the line itself matched Pattern; false marks a
	// context line.
	Hit bool
}

// DiskSearchResult is one content search's output. Named with a Disk
// prefix (unlike ListResult/FindResult) because the unqualified
// SearchResult name is already taken by the unrelated web-search tool's
// result type in this package (search.go).
type DiskSearchResult struct {
	// Lines may arrive in ANY order and a (Path, Line) pair MAY repeat
	// (ripgrep emits a line once per role: context of one hit, match of
	// the next). The caller deduplicates and makes Hit sticky
	// (fsGrepFileHits.add), so a provider need not.
	Lines []SearchLine
}

// diskOrOS normalises a possibly-nil DiskProvider to the real filesystem.
// nil is NOT a valid stored provider value — it means "use the real
// disk", so every fs_* constructor calls this once and captures the
// result, and no per-call nil check is needed anywhere below.
func diskOrOS(d DiskProvider) DiskProvider {
	if d == nil {
		return OSDisk()
	}
	return d
}

// osDisk is the stateless, real-filesystem DiskProvider: the exact
// os/filepath and search calls the fs_* family made before the provider
// seam existed, with no behavioural difference whatsoever. It has no
// state, so it is safe to hand out as a package-level singleton.
type osDisk struct{}

// osDiskMarker identifies the canonical real-filesystem provider without
// using interface equality as the identity mechanism. The unexported method
// prevents providers outside this package from accidentally claiming the
// canonical identity.
type osDiskMarker interface {
	isOSDisk()
}

func (osDisk) isOSDisk() {}

var osDiskSingleton DiskProvider = osDisk{}

// OSDisk returns the real-filesystem DiskProvider: the default every
// fs_* constructor falls back to when no DiskProvider is supplied.
func OSDisk() DiskProvider { return osDiskSingleton }

// HasDiskProvider reports whether d is a non-nil interface value containing a
// provider. The type switch keeps this presence check separate from the
// canonical-identity check below; it does not impose a comparability
// requirement on a provider's dynamic value.
func HasDiskProvider(d DiskProvider) bool {
	switch d.(type) {
	case nil:
		return false
	default:
		return true
	}
}

// IsOSDisk reports whether d is the canonical real-filesystem provider by
// asserting the private marker interface. The marker is useful as an
// explicit identity predicate and avoids making identity depend on interface
// equality. It is not a workaround for a reachable panic in the former
// comparisons: comparison with nil is always safe, and a custom provider has
// a different dynamic type from the comparable osDisk value.
func IsOSDisk(d DiskProvider) bool {
	_, ok := d.(osDiskMarker)
	return ok
}

// IsCustomDiskProvider reports whether d is a non-nil provider other than
// the canonical real filesystem.
func IsCustomDiskProvider(d DiskProvider) bool {
	return HasDiskProvider(d) && !IsOSDisk(d)
}

func (osDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (osDisk) Lstat(_ context.Context, name string) (fs.FileInfo, error) {
	return os.Lstat(name)
}

func (osDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	return filepath.EvalSymlinks(name)
}

func (osDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	return os.Open(name)
}

func (osDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	return os.ReadFile(name)
}

func (osDisk) MkdirAll(_ context.Context, dir string, perm fs.FileMode) error {
	return os.MkdirAll(dir, perm)
}

func (osDisk) WriteFile(_ context.Context, name string, data []byte, perm fs.FileMode) error {
	return fsext.AtomicWriteFile(name, data, perm)
}

func (osDisk) Remove(_ context.Context, name string) error {
	return os.Remove(name)
}

func (osDisk) List(_ context.Context, req ListRequest) (ListResult, error) {
	entries, truncated, err := fsext.ListDirectory(req.Dir, req.IgnorePatterns, req.Depth, req.Limit)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Entries: entries, Truncated: truncated}, nil
}

func (osDisk) Find(ctx context.Context, req FindRequest) (FindResult, error) {
	paths, truncated, err := globFiles(ctx, req.Pattern, req.Dir, req.Limit)
	if err != nil {
		return FindResult{}, err
	}
	return FindResult{Paths: paths, Truncated: truncated}, nil
}

// Search reproduces today's fs_grep dispatch verbatim: ripgrep invoked
// with -C N when available, falling back to the in-process regex walk
// when rg is unavailable or its run fails (fsGrepSearchContext, defined
// in fs_grep.go, already implements both engines and the handover
// between them — that dispatch and its clear(files) reset on fallback
// stay untouched here). Results land in a local map + budget and are
// then flattened into []SearchLine.
//
// Text normalisation is preserved verbatim, INCLUDING its existing
// inconsistency between the two engines: the rg parser stores
// strings.TrimSpace(...) (parseRipgrepContextStream) while the fallback
// stores strings.TrimSuffix(line, "\r") plus fallbackTruncateSuffix on
// overlong lines (scanFileWithContext) — leading whitespace survives on
// one path and not the other. Do not "clean this up": it would change
// rendered output that existing tests pin.
//
// req.MaxLines is only a hint (see SearchRequest.MaxLines): when > 0 it
// caps this call's local budget so the search does not do unbounded
// work; when 0, the local budget is effectively unlimited and the real
// truncation is left entirely to the caller, which re-applies its own
// budget (fsGrepFileHits.add) to the returned lines.
func (osDisk) Search(ctx context.Context, req SearchRequest) (DiskSearchResult, error) {
	remaining := req.MaxLines
	if remaining <= 0 {
		remaining = math.MaxInt
	}
	budget := fsGrepBudget{remaining: remaining}

	files := make(map[string]*fsGrepFileHits)
	if err := fsGrepSearchContext(ctx, req.Pattern, req.Dir, req.Include,
		req.ContextLines, files, &budget); err != nil {
		return DiskSearchResult{}, err
	}

	var lines []SearchLine
	for path, collector := range files {
		for lineNum, line := range collector.lines {
			lines = append(lines, SearchLine{
				Path: path,
				Line: lineNum,
				Text: line.text,
				Hit:  line.hit,
			})
		}
	}
	return DiskSearchResult{Lines: lines}, nil
}
