package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
)

// This file implements the shared batch runner for the scoped fs_* tool
// family: one place that owns the batch policy every fs_* tool inherits,
// the way multiedit.go is the one existing "N operations, one response,
// per-item outcome" tool.
//
// The policy, mirroring multiedit's two-phase shape:
//
//  1. Shape: the call is rejected whole (a level-1 error response,
//     before any per-item work) when it carries no items or more than
//     FSBatchMaxItems of them. Shape errors are wiring mistakes, not
//     policy decisions.
//  2. Pure preflight: every item is scope-checked and structurally
//     validated before ANY execution callback runs, so a batch that is
//     entirely out of scope does nothing and a partially out-of-scope
//     batch never executes a denied item. The runner itself does no
//     disk I/O during the preflight; a caller's Preflight may include
//     read-only existence checks (fs_write needs them to resolve
//     create-vs-overwrite) but must not mutate anything.
//  3. Best-effort execution: a failure on one item never blocks
//     unrelated items. Items whose resolved paths are equal form ONE
//     execution group and are handed to Execute together, so a caller
//     can apply N edits to one file in memory and write the file once
//     (one history version per file per call, as multiedit does).
//  4. The whole response is an error iff ZERO items succeeded —
//     multiedit's exact rule. StopTurn is never set here: a per-item
//     denial is model-correctable input, and only a whole-call
//     permissions.Request denial (the calling tool's concern) ends a
//     turn.
//  5. Every denied or failed item is slog.Warn-ed individually with
//     tool, session_id, path and op: the loggedTool wrapper
//     (internal/agent/logged_tool.go) only logs whole-response errors,
//     so without this a per-item denial inside an otherwise-successful
//     batch is invisible to the operator.
//
// Rendering split: the runner owns the deterministic summary (header
// line, one status line per item, the JSON metadata) and appends any
// per-item content blocks the Execute callback supplied (read-side
// tools render their own <file>...</file> blocks in view.go's shape;
// write-side tools normally supply none). Caps the runner cannot
// measure generically (grep matches per item, grep context radius) are
// reserved as named constants below for the calling tools to enforce.

// Per-item outcome statuses, shared by every fs_* tool.
const (
	// FSStatusOK marks an item whose operation succeeded.
	FSStatusOK = "ok"
	// FSStatusDenied marks an item refused by policy: an unresolvable
	// path or a folder-scope denial. The Error field says which rule
	// denied it.
	FSStatusDenied = "denied"
	// FSStatusFailed marks an item that was allowed but did not
	// succeed: structural validation or execution failure.
	FSStatusFailed = "failed"
	// FSStatusSkipped marks an item that was never attempted because a
	// batch-level budget was spent before its turn.
	FSStatusSkipped = "skipped"
	// FSStatusTruncated marks an item whose rendered block did not fit in
	// the remaining batch output budget.
	FSStatusTruncated = "truncated"
)

// Batch caps. FSBatchMaxItems is enforced by RunFSBatch as a whole-call
// shape error before the preflight; the rest are reserved for the
// calling tools to enforce per item, so every fs_* tool cites the same
// numbers instead of inventing its own.
const (
	// FSBatchMaxItems is the maximum number of items in one call.
	FSBatchMaxItems = 50
	// FSBatchMaxReadOutput is the total read output one call may emit
	// across all items (view.go's MaxViewSize, deliberately reused).
	// RunFSBatch enforces it on every rendered block: items scheduled after
	// the budget is spent are reported as skipped, and an oversized block is
	// reported as truncated without being emitted.
	FSBatchMaxReadOutput = MaxViewSize
	// FSBatchMaxGrepMatchesPerItem caps the matches one grep-style item
	// may report. Keep in step with grep.go's MaxResults default (100).
	FSBatchMaxGrepMatchesPerItem = 100
	// FSBatchMaxContextLines caps the context radius one grep-style
	// item may request, in lines on each side of a hit.
	FSBatchMaxContextLines = 50
)

// requestFSBatchPermissions authorizes every target path before any batch I/O.
func requestFSBatchPermissions(ctx context.Context, permissions permission.Service, sessionID, toolCallID, toolName, action, description string, paths []string, workingDir string, params any) (bool, error) {
	for _, path := range paths {
		granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
			SessionID:   sessionID,
			Path:        fsext.PathOrPrefix(path, workingDir),
			ToolCallID:  toolCallID,
			ToolName:    toolName,
			Action:      action,
			Description: description,
			Params:      params,
		})
		if err != nil || !granted {
			return granted, err
		}
	}
	return true, nil
}

// FSItemResult is one item's outcome inside a batch response, shared by
// every fs_* tool. It is rendered into the response text for the model
// and JSON-encoded into the response metadata for hosts and UIs.
type FSItemResult struct {
	// Index is the item's 0-based position in the batch.
	Index int `json:"index"`
	// Path is echoed exactly as the model sent it, not the resolved
	// path, so the model can correlate result line to input.
	Path string `json:"path"`
	// Op is the resolved FileOp (e.g. "create" vs "overwrite"),
	// resolved by the caller's Preflight, not by this runner.
	Op string `json:"op,omitempty"`
	// Status is one of the FSStatus* constants.
	Status string `json:"status"`
	// Error is the level-1 reason for a denied, failed or skipped
	// item; empty for ok items.
	Error string `json:"error,omitempty"`
	// Additions and Removals are write-side diff counts.
	Additions int `json:"additions,omitempty"`
	Removals  int `json:"removals,omitempty"`
	// Diff is write-side only. The calling tool owns both producing
	// and capping it; the runner carries it verbatim.
	Diff string `json:"diff,omitempty"`
}

// FSBatchResponseMetadata is the JSON metadata attached to every batch
// response. Failed counts every item that did not succeed (denied,
// failed or skipped).
type FSBatchResponseMetadata struct {
	Tool      string         `json:"tool"`
	Succeeded int            `json:"succeeded"`
	Failed    int            `json:"failed"`
	Items     []FSItemResult `json:"items"`
}

// FSItemOutcome is one item's fate as reported by an Execute callback.
// Outcomes must be returned in the same order as group.Items;
// RunFSBatch fills in each item's index and echoed path itself.
type FSItemOutcome struct {
	// Status must be FSStatusOK, FSStatusFailed or FSStatusSkipped.
	// Denial is the runner's decision, made during the preflight.
	Status string
	// Error is the level-1 reason when Status is not ok.
	Error string
	// Additions and Removals are write-side diff counts.
	Additions int
	Removals  int
	// Diff is write-side only. The calling tool owns producing and
	// capping it; the runner carries it verbatim into FSItemResult.
	Diff string
	// Block is the item's rendered content block (read-side tools: a
	// <file>...</file> block in view.go's shape). Blocks are appended
	// to the response text after the per-item summary lines, in item
	// order, and count against FSBatchMaxReadOutput. Write-side tools
	// normally leave it empty.
	Block string
}

// FSGroupItem is one item as handed to an Execute callback: its
// position in the batch, its path exactly as the model sent it, and the
// decoded item itself.
type FSGroupItem[I any] struct {
	Index   int
	RawPath string
	Item    I
}

// FSBatchGroup is every item of one call that resolved to the same
// absolute path, in batch order. It is the atomicity unit: a caller
// applies the group's items to the file in memory and writes the file
// once.
type FSBatchGroup[I any] struct {
	// Path is the resolved absolute path shared by all items.
	Path string
	// Items are the group's items in batch order.
	Items []FSGroupItem[I]
}

// FSPreflightFunc validates one item and resolves the FileOp its scope
// check needs. It runs before any execution and must not mutate the
// filesystem (read-only existence checks are expected: fs_write
// resolves create-vs-overwrite by statting the resolved path). Any
// error it returns fails that item alone as FSStatusFailed. absPath is
// the item's resolved absolute path, already symlink-resolved, so
// existence checks see what the OS would actually touch. ctx is the
// call's context, so a preflight that stats through a DiskProvider can
// honour cancellation.
type FSPreflightFunc[I any] func(ctx context.Context, item I, index int, absPath string) (op permission.FileOp, err error)

// FSExecuteFunc performs the real I/O for one file group. It must
// report exactly one outcome per group item, in order. Returning an
// error fails every item of the group with that error (a shared reason
// such as an unwritable file); a per-item outcome with FSStatusFailed
// keeps the item-level reason instead. To abort the whole call at
// level 3 of the tool error contract — infrastructure broken for good
// within this session, like a missing session id or a failed history
// write — return an *FSBatchAbortError wrapping the cause.
type FSExecuteFunc[I any] func(ctx context.Context, group FSBatchGroup[I]) ([]FSItemOutcome, error)

// FSBatchAbortError aborts the whole call at level 3 of the tool error
// contract (see the comment atop tools.go): it is returned as a Go
// error, which unwinds the agent loop, because a missing session id or
// a broken history service is invariant to retry — no input the model
// could send next would fix it.
type FSBatchAbortError struct{ Err error }

func (e *FSBatchAbortError) Error() string {
	return fmt.Sprintf("fs batch aborted: %v", e.Err)
}

func (e *FSBatchAbortError) Unwrap() error { return e.Err }

// FSBatch is one batch call: the decoded items plus the three
// caller-supplied functions that give them tool-specific meaning.
type FSBatch[I any] struct {
	// Tool is the calling tool's name, used in the summary header, the
	// metadata and the per-item log lines.
	Tool string
	// WorkingDir resolves the items' relative paths (via
	// resolveScopedPath, which also symlink-resolves them).
	WorkingDir string
	// Scope is the compiled matcher every item is checked against. The
	// zero value denies every item, so an uncompiled scope fails
	// closed.
	Scope permission.FolderScope
	// Items are the decoded batch items, in the order the model sent
	// them.
	Items []I
	// PathOf extracts an item's target path exactly as the model sent
	// it. It is echoed verbatim in the results and resolved for the
	// scope check.
	PathOf func(I) string
	// Preflight validates each item and resolves its FileOp.
	Preflight FSPreflightFunc[I]
	// Execute performs the I/O for one file group.
	Execute FSExecuteFunc[I]
	// Disk is the filesystem this call's path resolution runs against.
	// nil is the real disk (see diskOrOS): every fs_* constructor
	// normalises its own disk once and passes the same non-nil value
	// here, so path resolution and execution always agree on which
	// filesystem is in play.
	Disk DiskProvider
	// ResolvePath optionally overrides the default symlink-resolving path
	// resolver. The returned path is the path checked against Scope and used
	// for execution.
	ResolvePath func(ctx context.Context, disk DiskProvider, workingDir, raw string) (string, error)
}

// fsBatchGroupEntry pairs one scheduled item with its slot in the
// results slice.
type fsBatchGroupEntry[I any] struct {
	item     FSGroupItem[I]
	resultAt int
}

// fsBatchGroup is the runner-side form of a group under construction.
type fsBatchGroup[I any] struct {
	path    string
	members []fsBatchGroupEntry[I]
}

// RunFSBatch runs one fs_* batch call: shape check, pure preflight,
// group-by-path execution, per-item rendering. The response carries the
// model-facing summary in Content and the machine-readable
// FSBatchResponseMetadata JSON in Metadata; the wire protocol carries
// exactly one result per call and has no multi-part form, so both
// travel on the one fantasy.ToolResponse. It never sets StopTurn.
func RunFSBatch[I any](ctx context.Context, batch FSBatch[I]) (fantasy.ToolResponse, error) {
	if batch.PathOf == nil || batch.Preflight == nil || batch.Execute == nil {
		return fantasy.ToolResponse{}, errors.New("fs batch: PathOf, Preflight and Execute are required")
	}
	if len(batch.Items) == 0 {
		return fantasy.NewTextErrorResponse("at least one item is required"), nil
	}
	if len(batch.Items) > FSBatchMaxItems {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"too many items: %d (maximum %d per call)", len(batch.Items), FSBatchMaxItems)), nil
	}

	disk := diskOrOS(batch.Disk)
	results := make([]FSItemResult, len(batch.Items))
	groups := make([]fsBatchGroup[I], 0, len(batch.Items))
	byPath := make(map[string]int, len(batch.Items))

	// Phase 1: pure preflight. Nothing on disk is touched: each item is
	// resolved, validated and scope-checked, and survivors are grouped
	// by resolved path.
	for i, item := range batch.Items {
		result := FSItemResult{Index: i, Path: batch.PathOf(item)}
		results[i] = result

		resolver := batch.ResolvePath
		if resolver == nil {
			resolver = resolveScopedPath
		}
		abs, err := resolver(ctx, disk, batch.WorkingDir, result.Path)
		if err != nil {
			// Fail closed: a path that cannot be resolved cannot be
			// judged safe.
			result.Status = FSStatusDenied
			result.Error = err.Error()
			results[i] = result
			logFSItemProblem(ctx, batch.Tool, result)
			continue
		}

		op, err := batch.Preflight(ctx, item, i, abs)
		if err != nil {
			result.Status = FSStatusFailed
			result.Error = err.Error()
			results[i] = result
			logFSItemProblem(ctx, batch.Tool, result)
			continue
		}
		result.Op = string(op)

		if err := batch.Scope.Check(abs, op); err != nil {
			result.Status = FSStatusDenied
			var denied *permission.ScopeDeniedError
			if errors.As(err, &denied) {
				result.Error = denied.Reason
			} else {
				result.Error = err.Error()
			}
			results[i] = result
			logFSItemProblem(ctx, batch.Tool, result)
			continue
		}

		// Scheduled for execution. Skipped is what remains if the
		// read-output budget is spent before the item's group runs.
		result.Status = FSStatusSkipped
		results[i] = result

		at, seen := byPath[abs]
		if !seen {
			at = len(groups)
			byPath[abs] = at
			groups = append(groups, fsBatchGroup[I]{path: abs})
		}
		groups[at].members = append(groups[at].members, fsBatchGroupEntry[I]{
			item:     FSGroupItem[I]{Index: i, RawPath: result.Path, Item: item},
			resultAt: i,
		})
	}

	// Phase 2: best-effort execution, one group at a time, in
	// first-appearance order.
	blocks := make(map[int]string, len(batch.Items))
	emitted := 0
	for _, group := range groups {
		if emitted >= FSBatchMaxReadOutput {
			for _, member := range group.members {
				skipped := results[member.resultAt]
				skipped.Status = FSStatusSkipped
				skipped.Error = fmt.Sprintf(
					"read-output budget exhausted (%d bytes per call)", FSBatchMaxReadOutput)
				results[member.resultAt] = skipped
			}
			continue
		}

		items := make([]FSGroupItem[I], len(group.members))
		for j, member := range group.members {
			items[j] = member.item
		}
		outcomes, err := batch.Execute(ctx, FSBatchGroup[I]{Path: group.path, Items: items})
		if err != nil {
			var abort *FSBatchAbortError
			if errors.As(err, &abort) {
				return fantasy.ToolResponse{}, err
			}
			for _, member := range group.members {
				failed := results[member.resultAt]
				failed.Status = FSStatusFailed
				failed.Error = err.Error()
				results[member.resultAt] = failed
				logFSItemProblem(ctx, batch.Tool, failed)
			}
			continue
		}
		if len(outcomes) != len(group.members) {
			return fantasy.ToolResponse{}, fmt.Errorf(
				"fs batch: %s execute returned %d outcomes for %d items on %s",
				batch.Tool, len(outcomes), len(group.members), group.path)
		}
		for j, outcome := range outcomes {
			at := group.members[j].resultAt
			result := results[at]
			result.Status = outcome.Status
			result.Error = outcome.Error
			result.Additions = outcome.Additions
			result.Removals = outcome.Removals
			result.Diff = outcome.Diff
			results[at] = result
			if outcome.Block != "" {
				if len(outcome.Block) > FSBatchMaxReadOutput-emitted {
					result.Status = FSStatusTruncated
					result.Error = fmt.Sprintf(
						"read-output budget exhausted (%d bytes per call)", FSBatchMaxReadOutput)
					results[at] = result
				} else {
					blocks[at] = outcome.Block
					emitted += len(outcome.Block)
				}
			}
			if outcome.Status == FSStatusFailed || outcome.Status == FSStatusDenied {
				logFSItemProblem(ctx, batch.Tool, result)
			}
		}
	}

	// Phase 3: rendering. One deterministic summary line per item, then
	// any content blocks in item order.
	succeeded := 0
	for _, result := range results {
		if result.Status == FSStatusOK {
			succeeded++
		}
	}
	metadata := FSBatchResponseMetadata{
		Tool:      batch.Tool,
		Succeeded: succeeded,
		Failed:    len(results) - succeeded,
		Items:     results,
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d of %d items ok\n", batch.Tool, succeeded, len(results))
	for _, result := range results {
		b.WriteString(renderFSItemResult(result))
		b.WriteString("\n")
	}
	for i := range results {
		block, ok := blocks[i]
		if !ok {
			continue
		}
		b.WriteString(block)
		if !strings.HasSuffix(block, "\n") {
			b.WriteString("\n")
		}
	}

	resp := fantasy.NewTextResponse(b.String())
	if succeeded == 0 {
		resp.IsError = true
	}
	return fantasy.WithResponseMetadata(resp, metadata), nil
}

// renderFSItemResult renders one item's summary line. The status is
// padded to a fixed width so the lines align and stay greppable.
func renderFSItemResult(result FSItemResult) string {
	line := fmt.Sprintf("[%d] %-8s", result.Index, result.Status)
	if result.Status == FSStatusOK {
		switch {
		case result.Op != "" && (result.Additions != 0 || result.Removals != 0):
			return line + fmt.Sprintf("%s (%s, +%d/-%d)",
				result.Path, result.Op, result.Additions, result.Removals)
		case result.Op != "":
			return line + fmt.Sprintf("%s (%s)", result.Path, result.Op)
		default:
			return line + result.Path
		}
	}
	return line + fmt.Sprintf("%s: %s", result.Path, result.Error)
}

// logFSItemProblem warns one denied or failed item individually: the
// loggedTool wrapper only logs whole-response errors, so a per-item
// denial inside an otherwise-successful batch would otherwise be
// invisible to the operator.
func logFSItemProblem(ctx context.Context, tool string, result FSItemResult) {
	slog.Warn("Filesystem batch item "+result.Status,
		"tool", tool,
		"session_id", GetSessionFromContext(ctx),
		"path", result.Path,
		"op", result.Op,
		"error", result.Error,
	)
}

// resolveScopedPath resolves a raw (absolute or workingDir-relative) path
// into an absolute, symlink-resolved form suitable for
// permission.FolderScope.Check (or for pre-resolving a
// FolderScopeEntry.Dir before compilation).
//
// Only the longest EXISTING prefix of the path is symlink-resolved; the
// non-existent tail is appended unchanged. This produces a deliberate
// asymmetry: a create on a not-yet-existing file is judged by its
// RESOLVED PARENT directory (catching a symlinked parent that escapes a
// granted scope), while a read/write/delete on an existing file is
// judged by its own fully-resolved path (catching a symlink pointing
// outside the scope directly). Any ambiguity — an unreadable component,
// a file used as a directory, or a failed symlink evaluation — is an
// error, never a silently-produced path, so consumers fail closed.
//
// workingDir/raw are joined via filepathext.SmartJoin, then made
// absolute WITHOUT calling filepath.Abs whenever the joined result is
// already filepathext.SmartIsAbs (R5-6, P1 review finding). This
// matters because filepath.Abs is not a safe no-op on an
// already-absolute path on every OS: on Windows it unconditionally
// normalizes through syscall.FullPath, which silently re-roots a
// SmartIsAbs-but-driveless path (a leading "/" with no drive letter --
// e.g. a caller-supplied FolderScope entry spelled Unix-style against
// their own virtual DiskProvider) against the RUSH HOST PROCESS's own
// current drive/working directory. The same request could then resolve
// to a different real path if the host process's CWD changes between
// calls -- exactly the leak this guards against. A genuinely relative
// joined result with no workingDir at all has no safe root to resolve
// against, so it is rejected with an explicit error instead of silently
// falling back to the host process's CWD.
//
// LibraryVirtualRoot (this file) is no longer an example of the
// SmartIsAbs-but-driveless case above (R6-1 fix): it is now computed
// per-OS to satisfy the REAL filepath.IsAbs on every platform, so it
// takes the plain filepathext.SmartIsAbs(joined) branch just like any
// other genuinely absolute path. The guard immediately below is what
// actually keeps it inert: it fails closed before the first real
// disk.Stat whenever the resolved path would land under that sentinel
// AND disk is the real OSDisk.
func resolveScopedPath(ctx context.Context, disk DiskProvider, workingDir, raw string) (abs string, err error) {
	disk = diskOrOS(disk)
	joined := filepathext.SmartJoin(workingDir, raw)

	var cleaned string
	switch {
	case filepathext.SmartIsAbs(joined):
		cleaned = filepath.Clean(joined)
	case workingDir == "":
		return "", fmt.Errorf(
			"fs: resolve %q: relative path with no working directory to resolve it against", raw)
	default:
		cleaned, err = filepath.Abs(joined)
		if err != nil {
			return "", fmt.Errorf("fs: resolve %q: %w", raw, err)
		}
		cleaned = filepath.Clean(cleaned)
	}

	// R6-1 (P0 review finding): fail closed BEFORE the first real disk.Stat
	// call below when this resolution would touch the real filesystem
	// under the ephemeral library-mode sentinel. LibraryVirtualRoot is a
	// synthesized WorkingDir placeholder, never a real sandbox; a caller's
	// own DiskProvider is unaffected (see the helper's doc).
	if err := rejectRealDiskUnderLibraryVirtualRoot(disk, cleaned); err != nil {
		return "", err
	}

	// Walk up to the longest existing prefix. Anything other than a
	// clean miss (EACCES, ELOOP, ...) is unresolvable: fail closed.
	prefix := cleaned
	for {
		info, statErr := disk.Stat(ctx, prefix)
		if statErr == nil {
			if !info.IsDir() && prefix != cleaned {
				return "", fmt.Errorf(
					"fs: resolve %q: path component is a file, not a directory: %s",
					raw, prefix)
			}
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf("fs: resolve %q: stat %s: %w", raw, prefix, statErr)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			break
		}
		prefix = parent
	}

	resolvedPrefix, err := disk.EvalSymlinks(ctx, prefix)
	if err != nil {
		// A prefix we cannot evaluate cannot be judged safely.
		return "", fmt.Errorf("fs: resolve %q: evaluate symlinks in %s: %w",
			raw, prefix, err)
	}

	tail, err := filepath.Rel(prefix, cleaned)
	if err != nil {
		return "", fmt.Errorf("fs: resolve %q: %w", raw, err)
	}
	return filepath.Join(resolvedPrefix, tail), nil
}

// CanonicalizeFolderScopeSpec resolves spec.WorkingDir and every entry's
// Dir through resolveScopedPath's own longest-existing-prefix +
// EvalSymlinks algorithm — the SAME algorithm and the SAME disk provider
// every REQUESTED item path is resolved through before a
// permission.FolderScope ever sees it (R5-2, P0 security review finding).
//
// Production folder-scope compilation used to only filepath.Join a
// relative entry onto WorkingDir and filepath.Clean the result: no
// symlink resolution, no provider consultation. Every requested path,
// meanwhile, went through resolveScopedPath first. A scope's granted and
// denied roots and an actual request's item path could therefore land in
// two different namespaces — one lexical, one symlink-resolved — letting
// a symlinked deny carve-out silently stop matching while its broader
// parent grant kept matching. Calling this before
// permission.BuildFolderScope closes that gap: both sides of every
// FolderScope.Check are now produced by the same code path, in the same
// namespace.
//
// disk selects the filesystem roots are canonicalized against; nil means
// the real OS disk (see diskOrOS). A spec compiled with a caller-supplied
// DiskProvider must be canonicalized with that SAME provider — a
// durable-queue rebuild, which never carries a DiskProvider (see
// CallOptions.DiskProvider's doc comment), always canonicalizes with the
// real disk instead.
//
// Malformed entries (an empty Dir, or a relative Dir with an empty
// WorkingDir) are passed through UNCHANGED: permission.BuildFolderScope
// still owns rejecting them with its existing error text, and resolving
// either case here would either produce an uninformative error or
// silently invent the process's own working directory behind the host's
// back. Any OTHER resolution error (an unreadable path component, a
// failed symlink evaluation, ...) is returned to the caller, which MUST
// treat it exactly like a BuildFolderScope compile error: never
// substitute a partially-resolved spec, and never let the failure be
// mistaken for "no scope" — the zero FolderScope denies everything.
//
// Residual boundary, not addressed here (out of scope): this resolves
// every path once, before the matcher compiles and before any operation
// runs. A hostile local process that swaps a symlink between this
// resolution and the later filesystem operation (TOCTOU) is not defended
// against; closing that would need handle-relative filesystem
// primitives. Reopening the resolved pathname can therefore observe a
// different target.
func CanonicalizeFolderScopeSpec(ctx context.Context, disk DiskProvider, spec permission.FolderScopeSpec) (permission.FolderScopeSpec, error) {
	disk = diskOrOS(disk)

	out := permission.FolderScopeSpec{
		WorkingDir:       spec.WorkingDir,
		KeepCommandTools: spec.KeepCommandTools,
	}

	workingDir := strings.TrimSpace(spec.WorkingDir)
	if workingDir != "" {
		resolved, err := resolveScopedPath(ctx, disk, "", spec.WorkingDir)
		if err != nil {
			return permission.FolderScopeSpec{}, fmt.Errorf(
				"folder scope: resolve WorkingDir %q: %w", spec.WorkingDir, err)
		}
		out.WorkingDir = resolved
		workingDir = resolved
	}

	if len(spec.Entries) == 0 {
		return out, nil
	}
	out.Entries = make([]permission.FolderScopeEntry, len(spec.Entries))
	for i, entry := range spec.Entries {
		dir := strings.TrimSpace(entry.Dir)
		if dir == "" || (!filepath.IsAbs(dir) && workingDir == "") {
			// Leave it exactly as BuildFolderScope will see it: it owns
			// rejecting an empty Dir or a relative Dir with no
			// WorkingDir with its own error text.
			out.Entries[i] = entry
			continue
		}
		resolved, err := resolveScopedPath(ctx, disk, workingDir, entry.Dir)
		if err != nil {
			return permission.FolderScopeSpec{}, fmt.Errorf(
				"folder scope entry %d: resolve %q: %w", i, entry.Dir, err)
		}
		out.Entries[i] = permission.FolderScopeEntry{Dir: resolved, Ops: entry.Ops}
	}
	return out, nil
}

// FolderScopeToolOperations is the single operation-to-tool mapping used by
// the app permission gate and coordinator tool shaping.
func FolderScopeToolOperations() map[string]permission.FileOp {
	return map[string]permission.FileOp{
		FSListToolName:       permission.FileOpList,
		FSFindToolName:       permission.FileOpFind,
		FSGrepToolName:       permission.FileOpGrep,
		FSReadToolName:       permission.FileOpRead,
		FSReplaceToolName:    permission.FileOpReplace,
		FSWriteLinesToolName: permission.FileOpWriteLines,
		FSDeleteToolName:     permission.FileOpDelete,
	}
}

// FolderScopeToolNames returns the fs_* tools granted by scope. fs_write is
// included when either create or overwrite is granted.
func FolderScopeToolNames(scope permission.FolderScope) []string {
	operations := FolderScopeToolOperations()
	names := make([]string, 0, len(operations)+1)
	for name, op := range operations {
		if scope.Grants(op) {
			names = append(names, name)
		}
	}
	if scope.Grants(permission.FileOpCreate) || scope.Grants(permission.FileOpOverwrite) {
		names = append(names, FSWriteToolName)
	}
	slices.Sort(names)
	return names
}
