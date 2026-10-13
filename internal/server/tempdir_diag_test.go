package server

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// diagDataDir returns a data directory that proves the app tore its database
// down by the time the test is over. Cleanups run LIFO, so this one (registered
// first) runs after the app's own: any goroutine still inside the SQL driver
// or the db package that did not exist when the test started is a teardown
// that did not join its DB work, and a data dir that cannot be removed on the
// first attempt names its leftovers. Both are reported as test errors; the
// parent TempDir still guarantees the final cleanup.
func diagDataDir(t *testing.T) string {
	t.Helper()
	baseline := dbGoroutineStacks()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	t.Cleanup(func() {
		// A DB goroutine that is still alive 2s after the app teardown is not
		// winding down, it was never joined.
		var late map[string]string
		for deadline := time.Now().Add(2 * time.Second); ; {
			late = newStacks(baseline, dbGoroutineStacks())
			if len(late) == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(late) > 0 {
			t.Errorf("DB goroutines alive after app teardown:\n%s", joinStacks(late))
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("first remove of data dir failed: %v; left behind:\n%s", err, listLeftovers(dir))
		}
	})
	return dir
}

var goroutineHeader = regexp.MustCompile(`^goroutine (\d+) \[`)

// dbGoroutineStacks returns the stacks of goroutines that sit in the SQL
// driver or the db package, keyed by goroutine id.
func dbGoroutineStacks() map[string]string {
	var all strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&all, 2)
	out := map[string]string{}
	for _, g := range strings.Split(all.String(), "\n\n") {
		m := goroutineHeader.FindStringSubmatch(g)
		if m == nil {
			continue
		}
		if strings.Contains(g, "modernc.org/sqlite") || strings.Contains(g, "database/sql.") ||
			strings.Contains(g, "/internal/db.") {
			out[m[1]] = g
		}
	}
	return out
}

func newStacks(baseline, now map[string]string) map[string]string {
	out := map[string]string{}
	for id, g := range now {
		if _, ok := baseline[id]; !ok {
			out[id] = g
		}
	}
	return out
}

func joinStacks(m map[string]string) string {
	var b strings.Builder
	for _, g := range m {
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	return b.String()
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
