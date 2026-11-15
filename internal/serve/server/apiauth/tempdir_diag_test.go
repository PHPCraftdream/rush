package apiauth

// Local test-helper copies (S2, lane L2): apiauth_chain_test.go:32 calls
// logTempTreeBeforeRemoval, defined at HEAD in package server
// (tempdir_diag_test.go) with its helper describeTempTree (same file) and
// goroutinesMatching (api_batch_fixture_diag_test.go at HEAD). Verbatim
// copies; the servertest package (lanes L6/L7) will absorb them.

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
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
	_, globalDirActive := config.GlobalDir()
	fmt.Fprintf(&b, "=== temp dir %s just before removal (%s) ===\n", dir, now.Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "heartbeat.DefaultDir()=%q RUSH_HEARTBEAT_DIR=%q RUSH_GLOBAL_DIR=%q\n",
		heartbeat.DefaultDir(config.GlobalWorkspaceDir, globalDirActive), os.Getenv("RUSH_HEARTBEAT_DIR"), os.Getenv("RUSH_GLOBAL_DIR"))
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
	b.WriteString(goroutinesMatching(8000, "internal/session/heartbeat"))
	b.WriteString("\n")
	return b.String()
}

// goroutinesMatching returns the stacks of goroutines containing any key,
// capped for a readable CI log.
func goroutinesMatching(limit int, keys ...string) string {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var keep []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		for _, k := range keys {
			if strings.Contains(g, k) {
				keep = append(keep, g)
				break
			}
		}
	}
	out := fmt.Sprintf("goroutines matching %v: %d\n%s", keys, len(keep), strings.Join(keep, "\n\n"))
	if len(out) > limit {
		out = out[:limit] + "\n...(truncated)"
	}
	return out
}
