package server

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/stretchr/testify/require"
)

// A TempDir cleanup that fails with "directory is not empty" (seen once on a
// Windows runner, TestServeWSHelloFrame) says nothing about who was still
// writing. Cleanups run last-in-first-out, so logTempTreeBeforeRemoval, called
// right after the TempDir exists, records the tree and the writers just before
// RemoveAll. t.Log output is printed only when the test fails, which a failed
// cleanup makes it.
func logTempTreeBeforeRemoval(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() { t.Log(describeTempTree(dir)) })
}

const tempTreeEntryCap = 200

// describeTempTree lists what is in dir (with ages, so a file still being
// written shows as fresh) and what the process-wide heartbeat writer would
// resolve right now, plus the goroutines inside the heartbeat package.
func describeTempTree(dir string) string {
	var b strings.Builder
	now := time.Now()
	fmt.Fprintf(&b, "=== temp dir %s just before removal (%s) ===\n", dir, now.Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "heartbeat.DefaultDir()=%q RUSH_HEARTBEAT_DIR=%q RUSH_GLOBAL_DIR=%q\n",
		heartbeat.DefaultDir(), os.Getenv("RUSH_HEARTBEAT_DIR"), os.Getenv("RUSH_GLOBAL_DIR"))
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&b, "  walk error at %s: %v\n", path, err)
			return nil
		}
		if n++; n > tempTreeEntryCap {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(dir, path)
		info, infoErr := d.Info()
		if infoErr != nil {
			fmt.Fprintf(&b, "  %s (stat failed: %v)\n", rel, infoErr)
			return nil
		}
		kind := "file"
		if d.IsDir() {
			kind = "dir"
		}
		fmt.Fprintf(&b, "  %-4s %s size=%d modified %s ago\n", kind, rel, info.Size(), now.Sub(info.ModTime()).Round(time.Millisecond))
		return nil
	})
	if n > tempTreeEntryCap {
		fmt.Fprintf(&b, "  ...(more than %d entries)\n", tempTreeEntryCap)
	}
	b.WriteString(goroutinesMatching(8000, "internal/heartbeat"))
	b.WriteString("\n")
	return b.String()
}

func TestDescribeTempTreeShowsFilesAgesAndWriters(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "data", "heartbeat"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data", "heartbeat", "snap.json"), []byte("{}"), 0o644))

	report := describeTempTree(dir)
	require.Contains(t, report, "just before removal")
	require.Contains(t, report, "heartbeat.DefaultDir()=")
	require.Contains(t, report, filepath.Join("data", "heartbeat", "snap.json"))
	require.Contains(t, report, "size=2 modified")
	require.Contains(t, report, "dir  data")
	require.Contains(t, report, "goroutines matching")
}
