package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fsBatchTestItem is a stand-in for the per-item params a real fs_* tool
// will decode: those tools do not exist yet, so the batch runner is
// tested against this fake.
type fsBatchTestItem struct {
	Path         string
	Op           permission.FileOp
	PreflightErr error
	FailWith     string
	Block        string
	Additions    int
	Removals     int
}

// fsBatchTestPreflight is the fake preflight: echo the item's op, fail
// the item when PreflightErr is set.
func fsBatchTestPreflight(_ context.Context, item fsBatchTestItem, _ int, _ string) (permission.FileOp, error) {
	if item.PreflightErr != nil {
		return "", item.PreflightErr
	}
	if item.Op == "" {
		return permission.FileOpRead, nil
	}
	return item.Op, nil
}

// fsBatchTestExecute records every group it is handed — the recorder is
// how tests assert whether, and how often, real I/O would have happened
// — and reports per-item outcomes from the fake item fields.
func fsBatchTestExecute(recorded *[]FSBatchGroup[fsBatchTestItem]) FSExecuteFunc[fsBatchTestItem] {
	return func(_ context.Context, group FSBatchGroup[fsBatchTestItem]) ([]FSItemOutcome, error) {
		*recorded = append(*recorded, group)
		outcomes := make([]FSItemOutcome, len(group.Items))
		for i, member := range group.Items {
			item := member.Item
			if item.FailWith != "" {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: item.FailWith}
				continue
			}
			outcomes[i] = FSItemOutcome{
				Status:    FSStatusOK,
				Additions: item.Additions,
				Removals:  item.Removals,
				Block:     item.Block,
			}
		}
		return outcomes, nil
	}
}

// fsBatchTestScope builds a scope over workingDir, resolved the same way
// the runner resolves item paths, so the matcher compares
// resolved-to-resolved on every platform.
func fsBatchTestScope(t *testing.T, workingDir string, ops ...permission.FileOp) permission.FolderScope {
	t.Helper()
	resolved, err := resolveScopedPath(context.Background(), OSDisk(), workingDir, ".")
	require.NoError(t, err)
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: workingDir,
		Entries:    []permission.FolderScopeEntry{{Dir: resolved, Ops: ops}},
	})
	require.NoError(t, err)
	return scope
}

func fsBatchTestRun(ctx context.Context, workingDir string, scope permission.FolderScope, items []fsBatchTestItem, execute FSExecuteFunc[fsBatchTestItem]) (fantasy.ToolResponse, error) {
	return RunFSBatch(ctx, FSBatch[fsBatchTestItem]{
		Tool:       "fs_test",
		WorkingDir: workingDir,
		Scope:      scope,
		Items:      items,
		PathOf:     func(item fsBatchTestItem) string { return item.Path },
		Preflight:  fsBatchTestPreflight,
		Execute:    execute,
	})
}

func fsBatchTestMetadata(t *testing.T, resp fantasy.ToolResponse) FSBatchResponseMetadata {
	t.Helper()
	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	return meta
}

func TestFSBatchMixedOutcomes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blocker.txt"), []byte("x"), 0o644))
	outsidePath := filepath.Join(other, "secret.txt")

	items := []fsBatchTestItem{
		{Path: "a.go", Op: permission.FileOpOverwrite, Additions: 3, Removals: 1},
		{Path: filepath.Join("sub", "b.go"), Op: permission.FileOpCreate, FailWith: "old_string not found"},
		{Path: outsidePath, Op: permission.FileOpRead},
		{Path: filepath.Join("blocker.txt", "child"), Op: permission.FileOpRead},
	}

	var recorded []FSBatchGroup[fsBatchTestItem]
	resp, err := fsBatchTestRun(context.Background(), dir,
		fsBatchTestScope(t, dir, permission.FileOpRead, permission.FileOpOverwrite, permission.FileOpCreate),
		items, fsBatchTestExecute(&recorded))
	require.NoError(t, err)
	require.False(t, resp.IsError, "one succeeded item keeps the call successful")
	require.False(t, resp.StopTurn, "a per-item denial never ends the turn")

	meta := fsBatchTestMetadata(t, resp)
	require.Equal(t, "fs_test", meta.Tool)
	require.Equal(t, 1, meta.Succeeded)
	require.Equal(t, 3, meta.Failed)
	require.Len(t, meta.Items, 4)

	wantStatuses := []string{FSStatusOK, FSStatusFailed, FSStatusDenied, FSStatusDenied}
	for i, status := range wantStatuses {
		require.Equal(t, i, meta.Items[i].Index)
		require.Equal(t, status, meta.Items[i].Status)
	}
	require.Equal(t, "a.go", meta.Items[0].Path)
	require.Equal(t, "overwrite", meta.Items[0].Op)
	require.Equal(t, 3, meta.Items[0].Additions)
	require.Equal(t, 1, meta.Items[0].Removals)
	require.Equal(t, filepath.Join("sub", "b.go"), meta.Items[1].Path)
	require.Equal(t, "create", meta.Items[1].Op)
	require.Equal(t, "old_string not found", meta.Items[1].Error)
	require.Equal(t, outsidePath, meta.Items[2].Path, "the path is echoed exactly as sent")
	require.Equal(t, "outside every folder scope", meta.Items[2].Error)
	require.Contains(t, meta.Items[3].Error, "not a directory")

	require.Contains(t, resp.Content, "fs_test: 1 of 4 items ok")
	require.Contains(t, resp.Content, "[0] ok      a.go (overwrite, +3/-1)")
	require.Contains(t, resp.Content, fmt.Sprintf("[1] failed  %s: old_string not found", filepath.Join("sub", "b.go")))
	require.Contains(t, resp.Content, fmt.Sprintf("[2] denied  %s: outside every folder scope", outsidePath))
	require.Contains(t, resp.Content, "[3] denied  ")

	// Only the two allowed items reached the execute callback, one
	// group each; the denied ones never got there.
	require.Len(t, recorded, 2)
	require.Len(t, recorded[0].Items, 1)
	require.Equal(t, 0, recorded[0].Items[0].Index)
	require.Len(t, recorded[1].Items, 1)
	require.Equal(t, 1, recorded[1].Items[0].Index)
}

func TestFSBatchAllDeniedPerformsNoIO(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	items := []fsBatchTestItem{
		{Path: "a.go", Op: permission.FileOpRead},
		{Path: "b.go", Op: permission.FileOpRead},
	}

	var recorded []FSBatchGroup[fsBatchTestItem]
	// The zero FolderScope denies every path.
	resp, err := fsBatchTestRun(context.Background(), dir, permission.FolderScope{}, items, fsBatchTestExecute(&recorded))
	require.NoError(t, err)
	require.True(t, resp.IsError, "zero succeeded items make the whole call an error")
	require.False(t, resp.StopTurn)
	require.Empty(t, recorded, "denied items must never reach the execute callback")

	meta := fsBatchTestMetadata(t, resp)
	require.Equal(t, 0, meta.Succeeded)
	require.Equal(t, 2, meta.Failed)
	require.Len(t, meta.Items, 2)
	for _, item := range meta.Items {
		require.Equal(t, FSStatusDenied, item.Status)
	}
	require.Contains(t, resp.Content, "fs_test: 0 of 2 items ok")
}

func TestFSBatchGroupsSamePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	items := []fsBatchTestItem{
		{Path: "a.go", Op: permission.FileOpOverwrite},
		{Path: filepath.Join(".", "a.go"), Op: permission.FileOpOverwrite, FailWith: "overlapping range"},
		{Path: "b.go", Op: permission.FileOpRead},
	}

	var recorded []FSBatchGroup[fsBatchTestItem]
	resp, err := fsBatchTestRun(context.Background(), dir,
		fsBatchTestScope(t, dir, permission.FileOpRead, permission.FileOpOverwrite),
		items, fsBatchTestExecute(&recorded))
	require.NoError(t, err)
	require.False(t, resp.IsError)

	// Two items on one file formed ONE execution unit, in batch order;
	// the third item went to its own group.
	require.Len(t, recorded, 2)
	wantA, err := resolveScopedPath(context.Background(), OSDisk(), dir, "a.go")
	require.NoError(t, err)
	wantB, err := resolveScopedPath(context.Background(), OSDisk(), dir, "b.go")
	require.NoError(t, err)
	require.Equal(t, wantA, recorded[0].Path)
	require.Len(t, recorded[0].Items, 2)
	require.Equal(t, 0, recorded[0].Items[0].Index)
	require.Equal(t, "a.go", recorded[0].Items[0].RawPath)
	require.Equal(t, 1, recorded[0].Items[1].Index)
	require.Equal(t, filepath.Join(".", "a.go"), recorded[0].Items[1].RawPath)
	require.Equal(t, wantB, recorded[1].Path)
	require.Len(t, recorded[1].Items, 1)
	require.Equal(t, 2, recorded[1].Items[0].Index)

	// Best-effort within the group: one item's failure did not block
	// the other.
	meta := fsBatchTestMetadata(t, resp)
	require.Equal(t, 2, meta.Succeeded)
	require.Equal(t, 1, meta.Failed)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusFailed, meta.Items[1].Status)
	require.Equal(t, "overlapping range", meta.Items[1].Error)
	require.Equal(t, FSStatusOK, meta.Items[2].Status)
}

func TestFSBatchRejectsBadShape(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	scope := fsBatchTestScope(t, dir, permission.FileOpRead)

	preflightCalls := 0
	countingPreflight := func(ctx context.Context, item fsBatchTestItem, index int, absPath string) (permission.FileOp, error) {
		preflightCalls++
		return fsBatchTestPreflight(ctx, item, index, absPath)
	}
	var recorded []FSBatchGroup[fsBatchTestItem]

	// Over the cap: rejected whole, before the preflight runs.
	over := make([]fsBatchTestItem, FSBatchMaxItems+1)
	for i := range over {
		over[i] = fsBatchTestItem{Path: fmt.Sprintf("f%d.go", i)}
	}
	resp, err := RunFSBatch(context.Background(), FSBatch[fsBatchTestItem]{
		Tool: "fs_test", WorkingDir: dir, Scope: scope, Items: over,
		PathOf:    func(item fsBatchTestItem) string { return item.Path },
		Preflight: countingPreflight, Execute: fsBatchTestExecute(&recorded),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "too many items")
	require.Contains(t, resp.Content, fmt.Sprintf("maximum %d per call", FSBatchMaxItems))
	require.Equal(t, 0, preflightCalls)
	require.Empty(t, recorded)

	// Empty batch: rejected whole as well.
	resp, err = RunFSBatch(context.Background(), FSBatch[fsBatchTestItem]{
		Tool: "fs_test", WorkingDir: dir, Scope: scope, Items: nil,
		PathOf:    func(item fsBatchTestItem) string { return item.Path },
		Preflight: countingPreflight, Execute: fsBatchTestExecute(&recorded),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "at least one item is required")
	require.Equal(t, 0, preflightCalls)
	require.Empty(t, recorded)
}

func TestFSBatchReadBudgetSkipsRest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	items := []fsBatchTestItem{
		{Path: "big.txt", Block: strings.Repeat("x", FSBatchMaxReadOutput)},
		{Path: "next.txt"},
		{Path: "last.txt"},
	}

	var recorded []FSBatchGroup[fsBatchTestItem]
	resp, err := fsBatchTestRun(context.Background(), dir,
		fsBatchTestScope(t, dir, permission.FileOpRead), items, fsBatchTestExecute(&recorded))
	require.NoError(t, err)
	require.False(t, resp.IsError)

	// The budget is spent after the first group; the rest are skipped
	// without reaching the execute callback.
	require.Len(t, recorded, 1)

	meta := fsBatchTestMetadata(t, resp)
	require.Equal(t, 1, meta.Succeeded)
	require.Equal(t, 2, meta.Failed)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	for _, item := range meta.Items[1:] {
		require.Equal(t, FSStatusSkipped, item.Status)
		require.Contains(t, item.Error, "budget")
	}
	require.Contains(t, resp.Content, "fs_test: 1 of 3 items ok")
	require.Contains(t, resp.Content, "[1] skipped next.txt: read-output budget exhausted")
	require.Contains(t, resp.Content, "[2] skipped last.txt: read-output budget exhausted")
}

func TestFSBatchDenialIsLoggedPerItem(t *testing.T) {
	// Swaps the process-wide default logger: keep this test sequential.
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	items := []fsBatchTestItem{
		{Path: "a.go", Op: permission.FileOpRead},
		{Path: "b.go", Op: permission.FileOpOverwrite}, // read-only scope: denied
	}

	var recorded []FSBatchGroup[fsBatchTestItem]
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-fs-batch")
	resp, err := fsBatchTestRun(ctx, dir,
		fsBatchTestScope(t, dir, permission.FileOpRead), items, fsBatchTestExecute(&recorded))
	require.NoError(t, err)
	require.False(t, resp.IsError)

	logged := buf.String()
	require.Contains(t, logged, "Filesystem batch item denied")
	require.Contains(t, logged, "tool=fs_test")
	require.Contains(t, logged, "session_id=sess-fs-batch")
	require.Contains(t, logged, "path=b.go")
	require.Contains(t, logged, "op=overwrite")
	require.Contains(t, logged, "not granted")
	require.NotContains(t, logged, "path=a.go", "ok items are not logged as problems")
}

func TestFSBatchAbortErrorEndsCall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	aborting := func(_ context.Context, _ FSBatchGroup[fsBatchTestItem]) ([]FSItemOutcome, error) {
		return nil, &FSBatchAbortError{Err: errors.New("history db broken")}
	}
	resp, err := fsBatchTestRun(context.Background(), dir,
		fsBatchTestScope(t, dir, permission.FileOpRead),
		[]fsBatchTestItem{{Path: "a.go"}}, aborting)
	require.Error(t, err)
	require.Equal(t, fantasy.ToolResponse{}, resp)

	var abort *FSBatchAbortError
	require.ErrorAs(t, err, &abort)
	require.Equal(t, "history db broken", abort.Err.Error())
}

func TestFSBatchOutcomeCountMismatchIsCallerBug(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Two items on one path form one group; the fake callback reports
	// only one outcome — a caller contract violation, level 3.
	short := func(_ context.Context, _ FSBatchGroup[fsBatchTestItem]) ([]FSItemOutcome, error) {
		return []FSItemOutcome{{Status: FSStatusOK}}, nil
	}
	resp, err := fsBatchTestRun(context.Background(), dir,
		fsBatchTestScope(t, dir, permission.FileOpRead),
		[]fsBatchTestItem{{Path: "a.go"}, {Path: filepath.Join(".", "a.go")}}, short)
	require.Error(t, err)
	require.Equal(t, fantasy.ToolResponse{}, resp)
	require.Contains(t, err.Error(), "returned 1 outcomes for 2 items")
}
