package cmd

// Regression tests for the timestamp unit of `rush sessions audit`: every
// created_at / updated_at column of rush.db holds Unix SECONDS. The command
// shipped assuming milliseconds (#1162), and its fixtures were written in
// milliseconds too, so every test agreed with the bug while a real database
// showed 1970 dates and an empty --since window. These tests take the
// timestamps from the production services and queries instead of from a
// fixture the command's author controls.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestSessionsAudit_RealServiceTimestampsAreUnixSeconds creates a session
// through the real session service on a real, migrated database and audits
// it: the reported created_at must be "now" read as seconds, and the text
// output must print today's date.
//
// Revert-check: make auditFormatUnix read its argument as milliseconds
// (time.UnixMilli) and the text assertion goes red (a 1970 date).
func TestSessionsAudit_RealServiceTimestampsAreUnixSeconds(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	sess, err := session.NewService(db.New(conn), conn).Create(context.Background(), "audit-real-service")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, db.Release(dataDir))

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all", "--json")
	require.NoError(t, err, "stderr: %s", stderr)
	var reports []auditDBReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &reports))
	require.Len(t, reports, 1)
	require.Len(t, reports[0].Sessions, 1, "stdout: %s", stdout)
	row := reports[0].Sessions[0]
	require.Equal(t, sess.ID, row.ID)
	require.WithinDuration(t, time.Now(), time.Unix(row.CreatedAt, 0), time.Minute,
		"created_at is Unix seconds: reading it as seconds must give now")

	text, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all")
	require.NoError(t, err, "stderr: %s", stderr)
	require.Contains(t, text, time.Now().Format("2006-01-02"),
		"the session line must print today's date, not a 1970 one")
}

// TestAuditMessagesQuery_SinceIsUnixSeconds pins the --since parameter to the
// stored unit: "now minus the window" in Unix seconds.
//
// Revert-check: switch the parameter back to UnixMilli and the delta is
// about 1.8e9 seconds, so this test goes red.
func TestAuditMessagesQuery_SinceIsUnixSeconds(t *testing.T) {
	_, params := auditMessagesQuery(auditSchema{}, []string{"a"}, time.Hour)
	require.Len(t, params, 2)
	since, ok := params[1].(int64)
	require.True(t, ok)
	require.InDelta(t, time.Now().Add(-time.Hour).Unix(), since, 5)
}
