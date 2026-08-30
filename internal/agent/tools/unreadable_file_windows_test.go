//go:build windows

package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// makeUnreadableFileForTest locks the file against any other open so that
// os.Open fails with ERROR_SHARING_VIOLATION(32) while os.Stat still
// succeeds — the deterministic Windows way to reach view's residual read
// branch.
func makeUnreadableFileForTest(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o644))

	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ,
		0, // shareMode 0: no other open, not even another read, is allowed.
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	require.NoError(t, err, "CreateFile with shareMode 0 must succeed on the file we just wrote")
	t.Cleanup(func() { _ = windows.CloseHandle(h) })

	// The lock only does its job if it actually blocks a concurrent open --
	// confirmed unreliable on at least one GitHub-hosted windows-latest
	// runner (two independent CI runs read the file's real content straight
	// through the lock; per documented Win32 semantics a shareMode-0 handle
	// should make ANY subsequent CreateFile fail with
	// ERROR_SHARING_VIOLATION unconditionally, and this was not
	// reproducible locally after 30 runs on a real Windows workstation --
	// the runner-specific mechanism could not be pinned down without live
	// instrumentation on that host). Verify the precondition instead of
	// assuming it, so an environment where the lock doesn't hold skips
	// cleanly instead of asserting a false contract violation against
	// view.go, which is not the culprit.
	if f, openErr := os.Open(path); openErr == nil {
		_ = f.Close()
		t.Skip("shareMode-0 CreateFile lock did not block a concurrent os.Open in this environment " +
			"(known unreliable on some GitHub-hosted windows-latest runners); " +
			"skipping rather than asserting a false contract violation")
	}

	return path
}
