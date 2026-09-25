package tools

// Regression guard for a reproduced fatal crash: Edit's whole-file read
// (os.ReadFile in loadExistingFile) allocated the target's full logical
// size. A run pointed Edit at a scratch file with logical size
// 9,393,653,308 bytes; ReadFile tried to VirtualAlloc ~9.4 GB and the whole
// Rush process died with "fatal error: out of memory", killing the session,
// its lock lifecycle, and any pending completion reporting.
//
// The guard is a stat-time size check placed before the read, so the failure
// is a normal tool-error response the model can act on. These tests also pin
// the off-by-one boundary (exactly at the limit still edits) and the
// sparse-file case (a huge logical size with zero disk blocks must still
// fail fast, not by exhausting memory).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// runEditThroughTool drives Edit through its normal constructor/params path
// (JSON marshalled params, session in context, real permission/history/file
// mocks) so the guard is exercised exactly the way the agent calls it.
func runEditThroughTool(t *testing.T, dir string, params EditParams) fantasy.ToolResponse {
	t.Helper()

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "edit-size-guard-session")
	input, err := json.Marshal(params)
	require.NoError(t, err)

	tool := NewEditTool(
		&mockPermissionService{},
		&mockHistoryService{},
		mockFileTrackerService{},
		dir,
	)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "size-guard-call", Name: EditToolName, Input: string(input)})
	// The guard refuses model-correctable input, so it must come back as the
	// tool's standard text-error shape, never as a Go error that would end
	// the run (see the error contract in tools.go).
	require.NoError(t, err)
	return resp
}

// shrinkEditMaxFileSize narrows the edit read bound for the duration of a
// test and restores it afterwards, so the guard can be exercised with tiny
// files. NOT t.Parallel(): it mutates package-global editMaxFileSize, which
// the tool reads at call time (same seam pattern as jobOutputMaxWait).
func shrinkEditMaxFileSize(t *testing.T, limit int64) {
	t.Helper()
	original := editMaxFileSize
	editMaxFileSize = limit
	t.Cleanup(func() { editMaxFileSize = original })
}

func TestEdit_OversizedFile_FailsBeforeReading(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "big.txt")
	fileContent := "0123456789abcdefg" // 17 bytes
	require.NoError(t, os.WriteFile(filePath, []byte(fileContent), 0o644))

	// Limit of 16 against a 17-byte file: over by exactly one byte, so the
	// guard is the only thing that can explain the failure.
	shrinkEditMaxFileSize(t, 16)

	resp := runEditThroughTool(t, dir, EditParams{
		FilePath:   "big.txt",
		OldString:  "0123",
		NewString:  "X",
		ReplaceAll: false,
	})

	require.True(t, resp.IsError, "oversized file must be refused: %q", resp.Content)
	require.Contains(t, resp.Content, filePath)
	require.Contains(t, resp.Content, "17")
	require.Contains(t, resp.Content, strconv.FormatInt(16, 10))
	require.Contains(t, resp.Content, "too large")

	// Nothing was read or written: the file is byte-for-byte unchanged.
	after, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, fileContent, string(after))
}

func TestEdit_FileAtLimit_Succeeds(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "exact.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("0123456789abcde"), 0o644)) // 16 bytes

	// Exactly at the limit: the boundary is inclusive, so the edit must go
	// through (pins the comparison against off-by-one errors).
	shrinkEditMaxFileSize(t, 16)

	resp := runEditThroughTool(t, dir, EditParams{
		FilePath:  "exact.txt",
		OldString: "0123",
		NewString: "X",
	})

	require.False(t, resp.IsError, "a file exactly at the limit must still edit: %q", resp.Content)
	require.Contains(t, resp.Content, "Content replaced in file")

	after, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "X456789abcde", string(after))
}

func TestEdit_SparseHugeFile_FailsWithoutOOM(t *testing.T) {
	// Windows-only build detail: creating a sparse file needs
	// FSCTL_SET_SPARSE through DeviceIoControl, which Go does not expose;
	// everything else here is platform-neutral. The Unix path is the one
	// that reproduces the reported fatal run.
	if runtime.GOOS == "windows" {
		t.Skip("sparse-file extension needs FSCTL_SET_SPARSE, which Go does not expose; Unix path covered instead")
	}

	dir := t.TempDir()
	filePath := filepath.Join(dir, "sparse.bin")

	// A 3 GiB logical size with zero data blocks: Truncate only moves the
	// end-of-file marker, so setup allocates nothing and the test stays
	// fast. If the filesystem did not keep the file sparse, skip rather than
	// fill the disk with a real 3 GiB write.
	const hugeBytes int64 = 3 << 30 // 3 GiB
	f, err := os.Create(filePath)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(hugeBytes))
	stat, err := f.Stat()
	require.NoError(t, err)
	blocks := fileAllocatedBlocks(stat)
	require.NoError(t, f.Close())
	if blocks > 0 {
		t.Skipf("filesystem did not keep the file sparse (allocated %d blocks); skipping rather than filling the disk", blocks)
	}

	// The real limit, no override: this is the production path.
	start := time.Now()
	resp := runEditThroughTool(t, dir, EditParams{
		FilePath:  "sparse.bin",
		OldString: "anything",
		NewString: "nothing",
	})
	elapsed := time.Since(start)

	require.True(t, resp.IsError, "huge logical size must be refused: %q", resp.Content)
	require.Contains(t, resp.Content, filePath)
	require.Contains(t, resp.Content, "too large")
	require.Contains(t, resp.Content, strconv.FormatInt(hugeBytes, 10))
	require.Contains(t, resp.Content, strconv.FormatInt(editMaxFileSizeBytes, 10))
	// The regression this pins: pre-fix this path OOMs the process. It must
	// fail in milliseconds, not spend seconds or gigabytes getting there.
	require.Less(t, elapsed, 5*time.Second, "guard must fire before any read")
}

func TestEdit_MissingFile_ExistingBehaviorUnchanged(t *testing.T) {
	dir := t.TempDir()

	resp := runEditThroughTool(t, dir, EditParams{
		FilePath:  "does-not-exist.txt",
		OldString: "a",
		NewString: "b",
	})

	// Before the guard existed the stat failed with not-exist and the
	// existing "file not found" response was returned. The size check must
	// neither shadow nor double-report that.
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "file not found")
	require.NotContains(t, resp.Content, "too large")
}

// TestEditGate_ReturnsFalseUnderLimit pins the guard predicate's boundary
// directly: exactly-at-limit passes, one byte over fails.
func TestEditGate_ReturnsFalseUnderLimit(t *testing.T) {
	t.Parallel()

	resp, tooLarge := checkEditFileSize("f.txt", editMaxFileSizeBytes)
	require.False(t, tooLarge)
	require.False(t, resp.IsError)

	resp, tooLarge = checkEditFileSize("f.txt", editMaxFileSizeBytes+1)
	require.True(t, tooLarge)
	require.Contains(t, resp.Content, "too large")
	require.Contains(t, resp.Content, fmt.Sprintf("%d", editMaxFileSizeBytes+1))
	require.Contains(t, resp.Content, fmt.Sprintf("%d", editMaxFileSizeBytes))
}
