package app

// Task #616/P2-5 (2026-08-20 read-only release review): unit coverage for
// isSessionsIDConstraintError's two layers -- the typed *sqlite.Error fast
// path added by this task, and the pre-existing textual fallback -- plus a
// regression pinning that the unverified "constraint violation" wording
// (removed by this task; never actually produced by either vendored
// driver's own error-text code, see the function's own doc) is no longer
// treated as a match.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

// realUniqueConstraintErrorOnColumn provisions a real, temp-file-backed
// SQLite DB (in-memory won't reliably reproduce the driver's own error
// text/code plumbing the same way a real file-backed connection does — this
// mirrors internal/session/p589_cancellation_classifier_test.go's own
// realConstraintErrorForCancellationTest helper) and provokes a genuine
// UNIQUE constraint violation on a table/column named exactly `table.col`,
// so the returned error is driver-issued, not hand-constructed — the same
// discipline that test applies for its own *sqlite.Error fixture.
func realUniqueConstraintErrorOnColumn(t *testing.T, table, col string) error {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "unique-probe.db")
	rawDB, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { rawDB.Close() })

	_, err = rawDB.ExecContext(context.Background(), "CREATE TABLE "+table+" ("+col+" TEXT PRIMARY KEY)")
	require.NoError(t, err)
	_, err = rawDB.ExecContext(context.Background(), "INSERT INTO "+table+" ("+col+") VALUES ('dup')")
	require.NoError(t, err)

	_, dupErr := rawDB.ExecContext(context.Background(), "INSERT INTO "+table+" ("+col+") VALUES ('dup')")
	require.Error(t, dupErr, "the second insert must be rejected by the PRIMARY KEY constraint, or this test proves nothing")
	return dupErr
}

// TestIsSessionsIDConstraintError_TypedPath_SessionsID proves the typed fast
// path (errors.As against *sqlite.Error, masked &0xff == SQLITE_CONSTRAINT)
// recognizes a genuine, driver-issued PRIMARY KEY violation on sessions.id
// -- not a hand-built string -- and that the function still requires the
// textual "sessions.id" identity check to pass (a real driver error alone
// does not carry table/column identity via Code()).
func TestIsSessionsIDConstraintError_TypedPath_SessionsID(t *testing.T) {
	t.Parallel()

	err := realUniqueConstraintErrorOnColumn(t, "sessions", "id")

	var sqliteErr interface{ Code() int }
	require.True(t, errors.As(err, &sqliteErr), "test setup must produce a real *sqlite.Error, or this test proves nothing about the typed path")
	require.Equal(t, 19, sqliteErr.Code()&0xff, "must genuinely be SQLITE_CONSTRAINT (19)")

	require.True(t, isSessionsIDConstraintError(err), "a real driver-issued PRIMARY KEY violation on sessions.id must be recognized")
}

// TestIsSessionsIDConstraintError_TypedPath_OtherColumnNotSwallowed proves
// the typed code match ALONE is not sufficient: a genuine constraint
// violation on a DIFFERENT column (messages.id) must still be rejected,
// because SQLite's Code() cannot distinguish which column was violated --
// only the driver's message text can, and that check must still run even
// when the typed fast path matches.
func TestIsSessionsIDConstraintError_TypedPath_OtherColumnNotSwallowed(t *testing.T) {
	t.Parallel()

	err := realUniqueConstraintErrorOnColumn(t, "messages", "id")

	var sqliteErr interface{ Code() int }
	require.True(t, errors.As(err, &sqliteErr), "test setup must produce a real *sqlite.Error")
	require.Equal(t, 19, sqliteErr.Code()&0xff, "must genuinely be SQLITE_CONSTRAINT (19) -- proves the typed layer alone would have matched")

	require.False(t, isSessionsIDConstraintError(err), "a constraint violation on a DIFFERENT column must not be swallowed even though the typed code matches")
}

// TestIsSessionsIDConstraintError_TextualFallback_StillWorks proves the
// pre-existing textual path (the only path reachable for a driver whose
// error type is not *sqlite.Error, e.g. github.com/ncruces/go-sqlite3 on
// platforms connect_ncruces.go's build tag selects) still recognizes a
// hand-built message shaped like either real driver's actual output.
func TestIsSessionsIDConstraintError_TextualFallback_StillWorks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{"modernc UNIQUE format", "constraint failed: UNIQUE constraint failed: sessions.id (1555)", true},
		{"ncruces format", "sqlite3: constraint failed: UNIQUE constraint failed: sessions.id", true},
		{"other column not matched", "constraint failed: UNIQUE constraint failed: messages.id (1555)", false},
		{
			// Task #616/P2-5: the unverified "constraint violation" wording
			// must no longer match -- neither vendored driver's own
			// error-text code ever produces it (confirmed by reading both
			// drivers' source; see isSessionsIDConstraintError's own doc).
			// A plain generic error that happens to mention "constraint
			// violation" and "sessions.id" (e.g. from an unrelated code
			// path, or a future driver upgrade using different wording)
			// must NOT be treated as this specific, narrow race shape.
			"unverified 'constraint violation' wording no longer matches",
			"constraint violation on sessions.id",
			false,
		},
		{
			// Task #616/P2-6, RC11's own gap closed: a message containing
			// "sessions.id" WITHOUT any constraint marker at all (neither
			// "constraint failed" nor a typed *sqlite.Error) must not match.
			// The seventh review's RC11 found that removing the marker-guard
			// `if !strings.Contains(msg, "constraint failed") { return
			// false }` check broke NOTHING in the (at the time) existing
			// suite, because TestResolveSession_CreationRace_
			// OtherConstraintNotSwallowed's own fixture already contains
			// "constraint failed" (it exists to prove COLUMN narrowing, not
			// marker presence) -- so the marker guard had no dedicated
			// regression coverage of its own. This case is that coverage:
			// without the guard, any error merely naming "sessions.id" in
			// its text (a hypothetical unrelated error -- e.g. a future log
			// line, a different exception mentioning the column in passing)
			// would be misclassified as this specific creation-race shape
			// and silently trigger a re-Get, exactly the "swallow on any
			// error" defect class task #605's own commit message warned
			// against repeating.
			"sessions.id present but no constraint marker at all is not matched",
			"some unrelated failure while touching sessions.id",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isSessionsIDConstraintError(errors.New(tt.msg))
			require.Equal(t, tt.want, got)
		})
	}
}

// TestIsSessionsIDConstraintError_Nil proves the nil guard is preserved.
func TestIsSessionsIDConstraintError_Nil(t *testing.T) {
	t.Parallel()
	require.False(t, isSessionsIDConstraintError(nil))
}
