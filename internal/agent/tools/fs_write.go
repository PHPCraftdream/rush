package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
	"github.com/PHPCraftdream/rush/internal/kit/text/diff"
	"github.com/PHPCraftdream/rush/internal/store/filetracker"
	"github.com/PHPCraftdream/rush/internal/store/history"
)

// fs_write is the batch-capable member of the scoped fs_* write family:
// N files, one permission request per target, per-item outcomes via RunFSBatch, and
// one atomic write plus one history version per distinct file. It reuses
// edit.go's commitFileChange so durability, history and read-tracking
// behave exactly like the single-file write path.

//go:embed fs_write.md
var fsWriteDescription string

type FSWriteItem struct {
	Path       string `json:"path" description:"File path, absolute or relative to the working directory"`
	Content    string `json:"content" description:"Complete new content of the file"`
	CreateOnly bool   `json:"create_only,omitempty" description:"Fail this item if the file already exists (create, never overwrite)"`
	// AllowShrink: see the shrink guard in write_shrink_guard.go.
	AllowShrink bool `json:"allow_shrink,omitempty" description:"Set true only to deliberately replace a large existing file with much smaller content (refused otherwise: use fs_replace or fs_write_lines for partial changes)"`
}

type FSWriteParams struct {
	Items []FSWriteItem `json:"items" description:"Files to write. Each item is validated and reported independently; existing files are overwritten unless create_only is set"`
}

type FSWritePermissionsParams struct {
	Paths []string `json:"paths"`
}

const FSWriteToolName = "fs_write"

func NewFSWriteTool(scope permission.FolderScope, permissions permission.Service, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSWriteToolName,
		fsWriteDescription,
		func(ctx context.Context, params FSWriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if len(params.Items) == 0 {
				return fantasy.NewTextErrorResponse("at least one item is required"), nil
			}
			if len(params.Items) > FSBatchMaxItems {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"too many items: %d (maximum %d per call)", len(params.Items), FSBatchMaxItems)), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}

			joinedPaths := make([]string, len(params.Items))
			for i, item := range params.Items {
				if item.Path == "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("items[%d]: path is required", i)), nil
				}
				joined := filepathext.SmartJoin(workingDir, item.Path)
				if err := CheckForbiddenWrite(joined); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				joinedPaths[i] = joined
			}

			// One whole-call gate, multiedit's precedent: every target path is
			// authorized before any batch work, so a denial ends the turn
			// before the runner touches the filesystem.
			p, err := requestFSBatchPermissions(ctx, permissions, sessionID, call.ID, FSWriteToolName, "write", fmt.Sprintf("write %d files", len(params.Items)), joinedPaths, workingDir, FSWritePermissionsParams{Paths: joinedPaths})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			resp, err := RunFSBatch(ctx, FSBatch[FSWriteItem]{
				Tool:       FSWriteToolName,
				WorkingDir: workingDir,
				Scope:      scope,
				Items:      params.Items,
				Disk:       disk,
				PathOf: func(i FSWriteItem) string {
					return i.Path
				},
				Preflight: fsWritePreflight(disk),
				Execute: func(ctx context.Context, group FSBatchGroup[FSWriteItem]) ([]FSItemOutcome, error) {
					return fsWriteExecuteGroup(ctx, scope, files, filetracker, workingDir, disk, group)
				},
			})
			return resp, err
		},
	)
}

// fsWritePreflight resolves each item's operation without touching the
// filesystem beyond read-only stats, so the runner's scope check sees
// create-vs-overwrite exactly as execution will.
func fsWritePreflight(disk DiskProvider) FSPreflightFunc[FSWriteItem] {
	return func(ctx context.Context, item FSWriteItem, _ int, absPath string) (permission.FileOp, error) {
		info, err := disk.Stat(ctx, absPath)
		switch {
		case err == nil:
			if info.IsDir() {
				return "", fmt.Errorf("path is a directory, not a file: %s", absPath)
			}
			if item.CreateOnly {
				return "", fmt.Errorf("file already exists: %s", absPath)
			}
			return permission.FileOpOverwrite, nil
		case errors.Is(err, fs.ErrNotExist):
			return permission.FileOpCreate, nil
		default:
			return "", fmt.Errorf("cannot access %s: %w", absPath, err)
		}
	}
}

// fsWriteExecuteGroup applies every item of one same-path group in
// memory (last write wins) and writes the file once, so a group is one
// atomic write and one history version regardless of item count.
func fsWriteExecuteGroup(ctx context.Context, scope permission.FolderScope, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider, group FSBatchGroup[FSWriteItem]) ([]FSItemOutcome, error) {
	disk = diskOrOS(disk)
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil, &FSBatchAbortError{Err: errors.New("session_id is required")}
	}

	outcomes := make([]FSItemOutcome, len(group.Items))

	info, statErr := disk.Stat(ctx, group.Path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("cannot access %s: %v", group.Path, statErr),
			}
		}
		return outcomes, nil
	}

	if exists {
		if !info.IsDir() {
			if resp, tooLarge := checkEditFileSize(group.Path, info.Size()); tooLarge {
				for i := range outcomes {
					outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
				}
				return outcomes, nil
			}
		}
	}

	op := permission.FileOpCreate
	if exists {
		op = permission.FileOpOverwrite
	}

	// Defense in depth: the runner scope-checked each item's full file
	// path during preflight, but MkdirAll below is the step that
	// actually creates directories on disk, so re-check the resolved
	// parent at mutation time. This closes the case where scope was
	// narrowed between check and write, or where a direct caller (as
	// opposed to the runner) hands in a group whose parent was never
	// granted at all.
	denyAll := func(err error) ([]FSItemOutcome, error) {
		reason := err.Error()
		var denied *permission.ScopeDeniedError
		if errors.As(err, &denied) {
			reason = denied.Reason
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: reason}
		}
		return outcomes, nil
	}
	if err := scope.Check(filepath.Dir(group.Path), op); err != nil {
		return denyAll(err)
	}
	// When the file is new, MkdirAll also creates every missing
	// ancestor above the parent, and creating those directories is a
	// scope escape the file-path check cannot see, so each missing
	// ancestor gets its own check up to the first existing one.
	if !exists {
		for dir := filepath.Dir(group.Path); ; {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			if _, err := disk.Stat(ctx, parent); err == nil {
				break
			}
			if err := scope.Check(parent, op); err != nil {
				return denyAll(err)
			}
			dir = parent
		}
	}

	if !exists {
		if err := disk.MkdirAll(ctx, filepath.Dir(group.Path), 0o755); err != nil {
			if osFailureIsFatal(err) {
				return nil, &FSBatchAbortError{Err: fmt.Errorf("error creating directory: %w", err)}
			}
			return nil, fmt.Errorf("cannot create the parent directory for %s: %v", group.Path, err)
		}
	}

	// The old content is the undo/diff baseline commitFileChange relies
	// on. A failed read after Stat already reported the file exists
	// (transient provider error, permission race, ...) must never be
	// silently treated as "empty": that would make the write look like
	// a from-scratch create and record a false history baseline. Fail
	// the group the same way a fatal/non-fatal MkdirAll error above
	// does, and return before WriteFile or commitFileChange run.
	oldContent := ""
	if exists {
		oldBytes, err := disk.ReadFile(ctx, group.Path)
		if err != nil {
			if osFailureIsFatal(err) {
				return nil, &FSBatchAbortError{Err: fmt.Errorf("error reading existing content of %s: %w", group.Path, err)}
			}
			return nil, fmt.Errorf("cannot read existing content of %s: %v", group.Path, err)
		}
		oldContent = string(oldBytes)
	}
	current := oldContent

	for i, member := range group.Items {
		if exists && member.Item.CreateOnly {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  "file already exists: " + group.Path,
			}
			continue
		}
		if member.Item.Content == current {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("File %s already contains the exact content. No changes made.", group.Path),
			}
			continue
		}
		// Baseline is the larger of disk and in-call size: items must not compound.
		baseline := max(len(oldContent), len(current))
		if exists && !member.Item.AllowShrink && writeWouldShrinkFile(baseline, len(member.Item.Content)) {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  writeShrinkRefusal(FSWriteToolName, "fs_replace or fs_write_lines", group.Path, baseline, len(member.Item.Content)),
			}
			continue
		}
		diffText, additions, removals := diff.GenerateDiff(
			current, member.Item.Content, strings.TrimPrefix(group.Path, workingDir))
		outcomes[i] = FSItemOutcome{
			Status:    FSStatusOK,
			Additions: additions,
			Removals:  removals,
			Diff:      capFSDiff(diffText),
		}
		current = member.Item.Content
	}

	// Net-zero across the group: items cancelled each other out, so
	// the disk must stay untouched and no item can be reported as ok.
	if current == oldContent {
		for i := range outcomes {
			if outcomes[i].Status == FSStatusOK {
				outcomes[i] = FSItemOutcome{
					Status: FSStatusFailed,
					Error:  "no changes made - the file content is identical after applying all items",
				}
			}
		}
		return outcomes, nil
	}

	editCtx := editContext{
		ctx:         ctx,
		permissions: nil,
		files:       files,
		filetracker: filetracker,
		workingDir:  workingDir,
		disk:        disk,
	}
	resp, err := commitFileChange(editCtx, sessionID, group.Path, oldContent, current)
	if err != nil {
		// commitFileChange only returns an error for level-3
		// failures: a fatal write error or a history-service
		// failure.
		return nil, &FSBatchAbortError{Err: err}
	}
	if resp.IsError {
		for i := range outcomes {
			if outcomes[i].Status == FSStatusOK {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
			}
		}
	}
	return outcomes, nil
}

// fsMaxDiffBytes caps the diff text one item reports. The batch runner
// carries FSItemOutcome.Diff verbatim, so the producing tool owns the cap.
const fsMaxDiffBytes = 8 * 1024

// capFSDiff truncates a diff to fsMaxDiffBytes at a line boundary, with an
// explicit truncation marker so the model can see the diff was cut.
func capFSDiff(diffText string) string {
	if len(diffText) <= fsMaxDiffBytes {
		return diffText
	}
	cut := diffText[:fsMaxDiffBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n... (diff truncated)"
}

// fs_write_lines is the batch-capable member of the scoped fs_* write
// family that edits line ranges: N ranges across multiple files, one
// permission request per target, per-item outcomes via RunFSBatch, and — for items
// sharing one file — in-memory application bottom-up with a single
// atomic write. It reuses edit.go's commitFileChange so durability,
// history and read-tracking behave exactly like the single-file edit
// path.

//go:embed fs_write_lines.md
var fsWriteLinesDescription string

type FSWriteLinesItem struct {
	Path      string `json:"path" description:"File path, absolute or relative to the working directory"`
	StartLine int    `json:"start_line" description:"First line of the range, 1-based and inclusive"`
	EndLine   int    `json:"end_line" description:"Last line of the range, 1-based and inclusive; use start_line - 1 for a zero-length range that inserts before start_line"`
	Content   string `json:"content" description:"Replacement lines joined with newlines; an empty string deletes the range. Do not add a trailing newline unless you want an empty line"`
}

type FSWriteLinesParams struct {
	Items []FSWriteLinesItem `json:"items" description:"Line-range edits. Items sharing one file must not overlap; overlapping items all fail, non-overlapping items apply bottom-up and the file is written once"`
}

type FSWriteLinesPermissionsParams struct {
	Paths []string `json:"paths"`
}

const FSWriteLinesToolName = "fs_write_lines"

func NewFSWriteLinesTool(scope permission.FolderScope, permissions permission.Service, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSWriteLinesToolName,
		fsWriteLinesDescription,
		func(ctx context.Context, params FSWriteLinesParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if len(params.Items) == 0 {
				return fantasy.NewTextErrorResponse("at least one item is required"), nil
			}
			if len(params.Items) > FSBatchMaxItems {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"too many items: %d (maximum %d per call)", len(params.Items), FSBatchMaxItems)), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}

			joinedPaths := make([]string, len(params.Items))
			for i, item := range params.Items {
				if item.Path == "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("items[%d]: path is required", i)), nil
				}
				joined := filepathext.SmartJoin(workingDir, item.Path)
				if err := CheckForbiddenWrite(joined); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				joinedPaths[i] = joined
			}

			// Every target path is authorized before any batch work, so a denial
			// ends the turn before the runner touches the filesystem.
			p, err := requestFSBatchPermissions(ctx, permissions, sessionID, call.ID, FSWriteLinesToolName, "write", fmt.Sprintf("write lines in %d files", len(params.Items)), joinedPaths, workingDir, FSWriteLinesPermissionsParams{Paths: joinedPaths})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			resp, err := RunFSBatch(ctx, FSBatch[FSWriteLinesItem]{
				Tool:       FSWriteLinesToolName,
				WorkingDir: workingDir,
				Scope:      scope,
				Items:      params.Items,
				Disk:       disk,
				PathOf: func(i FSWriteLinesItem) string {
					return i.Path
				},
				Preflight: fsWriteLinesPreflight,
				Execute: func(ctx context.Context, group FSBatchGroup[FSWriteLinesItem]) ([]FSItemOutcome, error) {
					return fsWriteLinesExecuteGroup(ctx, scope, files, filetracker, workingDir, disk, group)
				},
			})
			return resp, err
		},
	)
}

// fsWriteLinesPreflight validates each item's line numbers structurally:
// bounds that need the file length are checked during execution, which
// reads the file anyway, so preflight never touches the filesystem.
func fsWriteLinesPreflight(_ context.Context, item FSWriteLinesItem, _ int, _ string) (permission.FileOp, error) {
	if item.StartLine < 1 {
		return "", fmt.Errorf("start_line must be at least 1")
	}
	if item.EndLine < item.StartLine-1 {
		return "", fmt.Errorf("end_line must be at least start_line - 1 (a zero-length range inserts before start_line)")
	}
	return permission.FileOpWriteLines, nil
}

// fsWriteLinesExecuteGroup applies every item of one same-path group in
// memory (bottom-up, so each item's 1-based coordinates refer to the
// original numbering) and writes the file once, so a group is one
// atomic write and one history version regardless of item count.
func fsWriteLinesExecuteGroup(ctx context.Context, scope permission.FolderScope, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider, group FSBatchGroup[FSWriteLinesItem]) ([]FSItemOutcome, error) {
	disk = diskOrOS(disk)
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil, &FSBatchAbortError{Err: errors.New("session_id is required")}
	}

	outcomes := make([]FSItemOutcome, len(group.Items))

	info, statErr := disk.Stat(ctx, group.Path)
	isDir := statErr == nil && info.IsDir()
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("file not found: %s", group.Path),
			}
		}
		return outcomes, nil
	case isDir:
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("path is a directory, not a file: %s", group.Path),
			}
		}
		return outcomes, nil
	case statErr != nil:
		if osFailureIsFatal(statErr) {
			return nil, &FSBatchAbortError{Err: fmt.Errorf("failed to access file: %w", statErr)}
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error: fmt.Sprintf("Cannot access %s: %v. The OS rejected this path — common causes are no permission somewhere along the path, a path component that is a file rather than a directory, or a name too long for the filesystem. Nothing was read or modified. Try a different path or a corrected form of this one.",
					group.Path, statErr),
			}
		}
		return outcomes, nil
	}

	if resp, tooLarge := checkEditFileSize(group.Path, info.Size()); tooLarge {
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
		}
		return outcomes, nil
	}

	lastRead := filetracker.LastReadTime(ctx, sessionID, group.Path)
	if lastRead.IsZero() {
		notRead := "you must read the file before writing lines in it. Use the fs_read tool first"
		// When the scope for this call lacks the read op, the generic
		// read-before-write advice is impossible to follow, so the
		// error must say the scope itself makes it unachievable.
		if scope.Check(group.Path, permission.FileOpRead) != nil {
			notRead = "the folder scope for this call grants write_lines but not read, so the file can never be read first — this item can never succeed under the current scope"
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: notRead}
		}
		return outcomes, nil
	}

	modTime := info.ModTime().Truncate(time.Second)
	if modTime.After(lastRead) {
		slog.Warn("File was modified externally since last read, proceeding with line write", "file", group.Path, "mod_time", modTime.Format(time.RFC3339), "last_read", lastRead.Format(time.RFC3339))
		filetracker.RecordRead(ctx, sessionID, group.Path)
	}

	content, readErr := disk.ReadFile(ctx, group.Path)
	if readErr != nil {
		if osFailureIsFatal(readErr) {
			return nil, &FSBatchAbortError{Err: fmt.Errorf("failed to read file: %w", readErr)}
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error: fmt.Sprintf("Cannot read %s: %v. The file was found but could not be read — it may have been locked by another process, or the storage failed mid-read. Nothing was modified. Try the write again or on a different file.",
					group.Path, readErr),
			}
		}
		return outcomes, nil
	}
	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))

	// A file "a\nb\n" splits on "\n" into ["a", "b", ""]: the trailing
	// empty element is the position after the final newline and counts
	// as a line address, so valid line numbers are 1..n where n is the
	// split length. Inserting at start_line == n appends before the
	// trailing empty element, i.e. after the last real line.
	lines := strings.Split(oldContent, "\n")
	n := len(lines)

	// Pass 1: file-length bounds (structural bounds already failed in
	// preflight). Survivors are collected with their batch indexes.
	type lineRange struct {
		start, end int
		itemIndex  int
	}
	valid := make([]lineRange, 0, len(group.Items))
	for i, member := range group.Items {
		if member.Item.StartLine > n {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("start_line %d is beyond the end of the file (%d lines)", member.Item.StartLine, n),
			}
			continue
		}
		if member.Item.EndLine > n {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("end_line %d is beyond the end of the file (%d lines)", member.Item.EndLine, n),
			}
			continue
		}
		valid = append(valid, lineRange{start: member.Item.StartLine, end: member.Item.EndLine, itemIndex: i})
	}

	// Overlapping ranges are ALL rejected, not just one of each pair.
	// Pure best-effort would have suggested failing only the later
	// item, which is less lossy — but two edits fighting over the same
	// lines would make the final content depend on application order,
	// so the report would lie about what is on disk. Determinism beats
	// lossiness here: every member of every overlapping pair fails.
	for i := 0; i < len(valid); i++ {
		for j := i + 1; j < len(valid); j++ {
			if fsWriteLinesRangesOverlap(valid[i].start, valid[i].end, valid[j].start, valid[j].end) {
				outcomes[valid[i].itemIndex] = FSItemOutcome{
					Status: FSStatusFailed,
					Error: fmt.Sprintf("overlapping range with item [%d] (lines %d-%d)",
						valid[j].itemIndex, valid[j].start, valid[j].end),
				}
				outcomes[valid[j].itemIndex] = FSItemOutcome{
					Status: FSStatusFailed,
					Error: fmt.Sprintf("overlapping range with item [%d] (lines %d-%d)",
						valid[i].itemIndex, valid[i].start, valid[i].end),
				}
			}
		}
	}

	// Pass 2: apply the survivors. Descending start applies bottom-up,
	// so each edit's 1-based coordinates are computed against the
	// ORIGINAL numbering: a higher-start edit only touches positions at
	// or above its start-1, which lie strictly after any lower-start
	// edit's untouched prefix, so no edit's coordinates are ever
	// shifted by another.
	//
	// Descending end as the tiebreak puts a real range [s,e] before a
	// zero-length insertion anchored at the same s, so the insertion
	// lands above the rewritten block.
	applied := make([]lineRange, 0, len(valid))
	for _, r := range valid {
		// Overlap rejection already wrote a failed outcome; everyone
		// else still carries the zero status and survives to be
		// applied.
		if outcomes[r.itemIndex].Status == "" {
			applied = append(applied, r)
		}
	}
	sort.Slice(applied, func(a, b int) bool {
		if applied[a].start != applied[b].start {
			return applied[a].start > applied[b].start
		}
		return applied[a].end > applied[b].end
	})

	work := append([]string(nil), lines...)
	for _, r := range applied {
		item := group.Items[r.itemIndex].Item
		var insertLines []string
		if item.Content != "" {
			insertLines = strings.Split(item.Content, "\n")
		}
		before := strings.Join(work, "\n")
		newWork := append([]string(nil), work[:r.start-1]...)
		newWork = append(newWork, insertLines...)
		newWork = append(newWork, work[r.end:]...)
		work = newWork
		after := strings.Join(work, "\n")
		if after == before {
			// Covers empty-content zero-length ranges and identical
			// rewrites alike.
			outcomes[r.itemIndex] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  "no changes made",
			}
			continue
		}
		diffText, additions, removals := diff.GenerateDiff(
			before, after, strings.TrimPrefix(group.Path, workingDir))
		outcomes[r.itemIndex] = FSItemOutcome{
			Status:    FSStatusOK,
			Additions: additions,
			Removals:  removals,
			Diff:      capFSDiff(diffText),
		}
	}

	finalContent := strings.Join(work, "\n")

	// Nothing changed: every item already failed, so the disk must
	// stay untouched and no write is issued.
	if finalContent == oldContent {
		return outcomes, nil
	}

	writeContent := finalContent
	if isCrlf {
		writeContent, _ = fsext.ToWindowsLineEndings(writeContent)
	}

	editCtx := editContext{
		ctx:         ctx,
		permissions: nil,
		files:       files,
		filetracker: filetracker,
		workingDir:  workingDir,
		disk:        disk,
	}
	resp, err := commitFileChange(editCtx, sessionID, group.Path, oldContent, writeContent)
	if err != nil {
		// commitFileChange only returns an error for level-3
		// failures: a fatal write error or a history-service
		// failure.
		return nil, &FSBatchAbortError{Err: err}
	}
	if resp.IsError {
		// One atomic write per group: a refusal to write fails the
		// whole group, so every ok item is downgraded.
		for i := range outcomes {
			if outcomes[i].Status == FSStatusOK {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
			}
		}
	}
	return outcomes, nil
}

// fsWriteLinesRangesOverlap reports whether two 1-based inclusive line
// ranges overlap. e == s-1 means a zero-length insertion before s.
func fsWriteLinesRangesOverlap(s1, e1, s2, e2 int) bool {
	// Both insertions: only the same insertion point collides.
	if e1 < s1 && e2 < s2 {
		return s1 == s2
	}
	// One insertion: it collides only when its anchor line lies
	// strictly inside the other range — at the range's own first
	// line it lands before the block and survives, and after the
	// last line it survives too.
	if e1 < s1 {
		return s2 < s1 && s1 <= e2
	}
	if e2 < s2 {
		return s1 < s2 && s2 <= e1
	}
	// Two real ranges overlap when they share any line.
	return max(s1, s2) <= min(e1, e2)
}
