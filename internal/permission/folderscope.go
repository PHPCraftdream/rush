package permission

// This file implements the folder-scope matcher: the compiled policy
// that decides whether ONE filesystem operation on ONE absolute path is
// allowed for a call that was handed a scoped filesystem toolset.
//
// It mirrors the spec -> compile -> query shape of RunAllowlist
// (runallowlist.go): a user-facing spec is compiled once per call into
// an immutable, concurrency-safe value, and every query afterwards is
// pure string logic. Deliberate differences from BuildRunAllowlist:
//
//  1. Compilation is a hard error on ANY malformed entry.
//     BuildRunAllowlist logs a bad pattern and drops it, which is safe
//     there because dropping a bad GRANT only narrows access. A
//     folder-scope entry can just as easily be a deny CARVE-OUT (an
//     entry with no Ops that excludes a sub-tree), and silently
//     dropping a bad carve-out would WIDEN access. The compiler cannot
//     tell which the host meant, so it refuses the whole spec instead
//     of guessing.
//
//  2. The matcher does not hook Service.Request. A Request denial ends
//     the turn because it means "the operator said no"; a per-item
//     scope miss inside a batch call is model-correctable input (pick
//     another path), so it surfaces as a typed per-item error
//     (*ScopeDeniedError) instead. Tools consuming this matcher decide
//     their own Request behaviour on top of it.
//
//  3. The matcher is total and touches no disk I/O. Callers pass paths
//     that are ALREADY resolved to absolute form (symlink resolution
//     and working-directory joining belong to the caller's resolver);
//     the matcher only compares strings and fails closed on anything it
//     cannot relate to an entry (relative input, cross-volume paths,
//     paths outside every entry).
//
// The zero value FolderScope{} denies every operation on every path, so
// a forgotten-to-initialize scope can never degrade into
// "unrestricted". A nil *FolderScope on a call means "unscoped call"
// (legacy toolset) and is a distinct, caller-level decision.

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/filepathext"
)

// FileOp is one filesystem operation a folder scope can grant.
type FileOp string

const (
	// FileOpList grants directory listings.
	FileOpList FileOp = "list"
	// FileOpFind grants by-name file searches.
	FileOpFind FileOp = "find"
	// FileOpGrep grants by-content searches, including context radii.
	FileOpGrep FileOp = "grep"
	// FileOpRead grants file reads (whole, ranged, or around hits).
	FileOpRead FileOp = "read"
	// FileOpCreate grants creating a file that does not exist yet.
	FileOpCreate FileOp = "create"
	// FileOpOverwrite grants replacing the full content of an existing
	// file.
	FileOpOverwrite FileOp = "overwrite"
	// FileOpWriteLines grants writing a line range into a file.
	FileOpWriteLines FileOp = "write_lines"
	// FileOpReplace grants find-and-replace edits inside a file.
	FileOpReplace FileOp = "replace"
	// FileOpDelete grants deleting a file.
	FileOpDelete FileOp = "delete"
)

// knownFileOps is the closed set of operations BuildFolderScope
// accepts. Anything else in an entry's Ops is malformed and fails
// compilation: a typo'd operation can never grant what the host
// intended, and a silently ignored grant would narrow access without
// telling anyone.
var knownFileOps = map[FileOp]struct{}{
	FileOpList: {}, FileOpFind: {}, FileOpGrep: {}, FileOpRead: {},
	FileOpCreate: {}, FileOpOverwrite: {}, FileOpWriteLines: {},
	FileOpReplace: {}, FileOpDelete: {},
}

// FolderScopeEntry is one host-facing scope: a directory subtree plus
// the operations granted inside it. Dir is absolute or relative to
// FolderScopeSpec.WorkingDir ("." means the working directory itself).
// Ops empty means a deny carve-out: the subtree matches NO operations,
// which — because the longest matching entry wins — excludes it from
// every enclosing entry's grants.
type FolderScopeEntry struct {
	Dir string
	Ops []FileOp
}

// FolderScopeSpec is the user-facing, pre-compilation form of a folder
// scope set, mirroring RunAllowlistSpec. BuildFolderScope compiles it
// into a queryable FolderScope.
type FolderScopeSpec struct {
	// WorkingDir resolves relative Dir entries. It is set by the run
	// plumbing from the app's working directory; hosts never set it.
	WorkingDir string
	// Entries are the scopes. Any single malformed entry fails the
	// whole compilation (see BuildFolderScope).
	Entries []FolderScopeEntry
	// KeepCommandTools records that command-executing tools stay in the
	// scoped toolset. It is carried for the coordinator wiring and has
	// no effect on matching.
	KeepCommandTools bool
}

// compiledScopeEntry is one scope with its Dir resolved to an absolute,
// cleaned path and its Ops as a set. An empty ops set is the deny
// carve-out.
type compiledScopeEntry struct {
	dir string
	ops map[FileOp]struct{}
}

// FolderScope is the compiled, immutable, concurrency-safe matcher for
// a set of folder scopes. All methods are safe for concurrent use on a
// shared value because nothing mutates after BuildFolderScope returns.
// The zero value denies every operation on every path.
type FolderScope struct {
	// entries is sorted by descending Dir length so the FIRST entry
	// whose dir contains the path is the longest (deepest) match.
	entries []compiledScopeEntry
	// granted is the union of all entries' operations, for the coarse
	// "does any scope grant this" decision. Carve-outs contribute
	// nothing by construction.
	granted map[FileOp]struct{}
	// keepCommandTools carries the coordinator's decision about the
	// command-executing tools; see KeepsCommandTools.
	keepCommandTools bool
}

// BuildFolderScope compiles spec into a queryable FolderScope. Unlike
// BuildRunAllowlist, which drops a bad pattern and keeps the rest, ANY
// malformed entry fails the whole compilation and the returned value is
// the zero FolderScope (deny-everything). The asymmetry is deliberate:
// a folder-scope entry may be a deny carve-out rather than a grant, and
// dropping a malformed carve-out would silently WIDEN access instead of
// narrowing it — the compiler cannot tell which the host intended, so
// it refuses rather than guess.
//
// Malformed means: an empty Dir; a relative Dir when spec.WorkingDir is
// empty (relative entries would otherwise silently resolve against the
// process working directory behind the host's back); or an Ops value
// outside the known operation set.
func BuildFolderScope(spec FolderScopeSpec) (FolderScope, error) {
	out := FolderScope{
		granted:          make(map[FileOp]struct{}),
		keepCommandTools: spec.KeepCommandTools,
	}
	for i, entry := range spec.Entries {
		dir := strings.TrimSpace(entry.Dir)
		if dir == "" {
			return FolderScope{}, fmt.Errorf("folder scope entry %d: Dir is empty", i)
		}
		// alreadyAbsInThisNamespace treats dir as needing no join onto
		// WorkingDir in exactly two cases: (1) dir is genuinely
		// filepath.IsAbs (a real drive-letter/UNC/Unix-rooted path), or
		// (2) dir is only filepathext.SmartIsAbs (a driveless leading
		// "/" — real-absolute on Unix, Windows-ambiguous) AND
		// spec.WorkingDir is ITSELF in that same driveless-virtual
		// namespace (R5-6: tools.CanonicalizeFolderScopeSpec resolves
		// entries against a synthesized virtual root like
		// sdk.LibraryVirtualRoot, which is deliberately
		// SmartIsAbs-but-not-IsAbs on Windows, producing entries in that
		// same namespace).
		//
		// Using plain filepathext.SmartIsAbs(dir) alone here — without
		// also checking spec.WorkingDir's own namespace — was tried and
		// reverted: on Windows, filepathext.SmartIsAbs("/foo") is true
		// even under a REAL drive-rooted WorkingDir like `D:\project`,
		// so a leading-slash-style entry (`{Dir: "/foo"}`, a plausible
		// operator spelling meaning "the foo subdirectory") would stop
		// being joined onto WorkingDir and become a driveless, permanently
		// unreachable root — silently turning a deny carve-out inert
		// (the item path resolveScopedPath produces for any real request
		// is always drive-rooted, so it can never match a driveless
		// entry). That is the exact fail-open regression class R5-2
		// exists to prevent, reintroduced through a different mechanism;
		// confirmed via a scratch reproduction before this guard was
		// added. Gating on spec.WorkingDir's own namespace keeps every
		// REAL WorkingDir's leading-slash entries joining exactly as
		// before, while still accepting the R5-6 virtual-root case.
		alreadyAbsInThisNamespace := filepath.IsAbs(dir) ||
			(filepathext.SmartIsAbs(dir) && filepathext.SmartIsAbs(spec.WorkingDir) && !filepath.IsAbs(spec.WorkingDir))
		if !alreadyAbsInThisNamespace {
			if strings.TrimSpace(spec.WorkingDir) == "" {
				return FolderScope{}, fmt.Errorf(
					"folder scope entry %d: Dir %q is relative but WorkingDir is empty",
					i, entry.Dir)
			}
			dir = filepath.Join(spec.WorkingDir, dir)
		}
		dir = filepath.Clean(dir)

		ops := make(map[FileOp]struct{}, len(entry.Ops))
		for _, op := range entry.Ops {
			if _, known := knownFileOps[op]; !known {
				return FolderScope{}, fmt.Errorf(
					"folder scope entry %d (%s): unknown operation %q", i, dir, op)
			}
			ops[op] = struct{}{}
			out.granted[op] = struct{}{}
		}
		out.entries = append(out.entries, compiledScopeEntry{dir: dir, ops: ops})
	}

	// Longest Dir first: when entries nest, the deeper dir is always
	// the longer string, so a stable sort by length makes the first
	// match the deepest match. Equal-length entries cannot both contain
	// the same path, so their relative order only matters for duplicate
	// dirs, where spec order (first entry wins) is the documented tie.
	slices.SortStableFunc(out.entries, func(a, b compiledScopeEntry) int {
		return cmp.Compare(len(b.dir), len(a.dir))
	})
	return out, nil
}

// Grants reports whether ANY entry grants op — the coarse decision used
// to pick which scoped tools exist for a call at all. Carve-out entries
// grant nothing by construction. The zero value grants nothing.
func (s FolderScope) Grants(op FileOp) bool {
	_, ok := s.granted[op]
	return ok
}

// Check reports whether op on absResolvedPath is allowed by this scope.
// The path must already be resolved to absolute form by the caller; the
// matcher compares strings only and does no disk I/O. nil means
// allowed; any denial is a *ScopeDeniedError (match with errors.As)
// whose Reason says which rule denied the path.
//
// Matching picks the entry with the longest Dir containing the path — a
// path matches exactly ONE entry, never a union of overlapping ones, so
// an entry granting nothing under a permissive parent is a real
// exclusion. Containment is the codebase-standard predicate
// (filepath.Rel succeeds and the result does not start with ".."):
// a Rel error (e.g. cross-volume on Windows) and an escaping result
// both deny.
func (s FolderScope) Check(absResolvedPath string, op FileOp) error {
	entry, ok := s.match(absResolvedPath)
	if !ok {
		return &ScopeDeniedError{
			Path:   absResolvedPath,
			Op:     op,
			Reason: "outside every folder scope",
		}
	}
	if _, allowed := entry.ops[op]; allowed {
		return nil
	}
	if len(entry.ops) == 0 {
		return &ScopeDeniedError{
			Path:   absResolvedPath,
			Op:     op,
			Reason: fmt.Sprintf("inside deny-carved scope %s", entry.dir),
		}
	}
	return &ScopeDeniedError{
		Path:   absResolvedPath,
		Op:     op,
		Reason: fmt.Sprintf("operation %q is not granted by scope %s", op, entry.dir),
	}
}

// match returns the longest entry whose dir contains path, or false
// when no entry does. An empty path never matches; the input is cleaned
// so a path spelled with traversal still compares as what it denotes. A
// Rel of "." means the path IS the entry dir and matches it. The
// ".."-prefix test is the containment predicate already used by
// isInSkillsPath and fsext.HasPrefix; it can false-deny a segment
// literally named with a ".." prefix, which is the safe direction.
func (s FolderScope) match(path string) (compiledScopeEntry, bool) {
	if path == "" {
		return compiledScopeEntry{}, false
	}
	target := filepath.Clean(path)
	for _, entry := range s.entries {
		rel, err := filepath.Rel(entry.dir, target)
		if err != nil {
			// Different volumes (Windows drive letters, UNC vs drive)
			// cannot be related: this entry cannot contain the path.
			continue
		}
		if !strings.HasPrefix(rel, "..") {
			return entry, true
		}
	}
	return compiledScopeEntry{}, false
}

// Roots returns the directories that grant op, deepest first (the same
// order the matcher resolves), for use as default search or listing
// roots. A returned root may still contain deny-carved subtrees; every
// concrete path under it must still go through Check. Returns nil when
// no entry grants op.
func (s FolderScope) Roots(op FileOp) []string {
	var roots []string
	for _, entry := range s.entries {
		if _, ok := entry.ops[op]; ok {
			roots = append(roots, entry.dir)
		}
	}
	return roots
}

// KeepsCommandTools reports whether command-executing tools stay in the
// scoped toolset. It only carries the coordinator's decision and has no
// effect on matching.
func (s FolderScope) KeepsCommandTools() bool { return s.keepCommandTools }

// ScopeDeniedError is the typed denial returned by FolderScope.Check.
// It mirrors agentguard.DeniedError's shape: one typed error so a batch
// runner can render it per item and tests can match it with errors.As.
type ScopeDeniedError struct {
	// Path is the path that was denied, exactly as passed to Check.
	Path string
	// Op is the operation that was denied.
	Op FileOp
	// Reason is a human-readable explanation of which rule denied it.
	Reason string
}

// Error implements error with a stable, greppable prefix.
func (e *ScopeDeniedError) Error() string {
	return fmt.Sprintf("folderscope: denied %s of %q — %s", e.Op, e.Path, e.Reason)
}
