package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// newRebuildFixtureSession supplies the durable root required by queue replay.
func newRebuildFixtureSession(t *testing.T, coord *coordinator, title string) string {
	t.Helper()
	require.NotNil(t, coord.sessions)
	sess, err := coord.sessions.Create(t.Context(), title)
	require.NoError(t, err)
	require.Empty(t, sess.ParentSessionID)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("rebuild fixture: session_id=%s title=%s parent_session_id=%s", sess.ID, title, sess.ParentSessionID)
		}
	})
	return sess.ID
}
