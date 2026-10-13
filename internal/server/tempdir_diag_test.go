package server

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
		t.Errorf("remove data dir: %v; left behind:\n%s", err, listLeftovers(dir))
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
