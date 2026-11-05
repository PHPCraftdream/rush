package dbtest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func pragma(t *testing.T, path, name string) string {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Pins the pragmas: journal_mode "wal", synchronous 1 (NORMAL). A fixture that
// silently fell back to the driver defaults would bring the per-commit fsync
// stalls back.
func TestOpenAppliesProductionPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	if got := pragma(t, path, "journal_mode"); !strings.EqualFold(got, "wal") {
		t.Errorf("journal_mode = %q, want wal", got)
	}
	if got := pragma(t, path, "synchronous"); got != "1" {
		t.Errorf("synchronous = %q, want 1 (NORMAL)", got)
	}
}

func TestOpenHandlesPathsWithURISpecials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a#b%c.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
}
