package server

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"
)

// diagDataDir returns a data directory whose removal failure reports what is
// still inside it. t.TempDir only says "directory not empty"; this names the
// leftovers so a late writer (a connection reopened after ReleaseAll, a
// background logger) can be identified. The parent TempDir still guarantees
// the final cleanup.
func diagDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	t.Cleanup(func() {
		err := os.RemoveAll(dir)
		if err == nil {
			return
		}
		t.Errorf("remove data dir: %v; left behind:\n%s\nDB-touching goroutines:\n%s",
			err, listLeftovers(dir), dbGoroutines())
	})
	return dir
}

func listLeftovers(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&b, "  %s: walk error: %v\n", path, err)
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			fmt.Fprintf(&b, "  %s: stat error: %v\n", path, ierr)
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(&b, "  %s size=%d mtime=%s\n", rel, info.Size(), info.ModTime().Format("15:04:05.000"))
		return nil
	})
	return b.String()
}

// dbGoroutines returns the stacks of goroutines that sit in the SQL driver or
// the db package, i.e. whoever still holds the data dir's file handle.
func dbGoroutines() string {
	var all strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&all, 2)
	var b strings.Builder
	for _, g := range strings.Split(all.String(), "\n\n") {
		if strings.Contains(g, "modernc.org/sqlite") || strings.Contains(g, "database/sql.") ||
			strings.Contains(g, "/internal/db.") {
			b.WriteString(g)
			b.WriteString("\n\n")
		}
	}
	if b.Len() == 0 {
		return "  (none)\n"
	}
	return b.String()
}
