package agent

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestQuestionFixtureCleanupCancelsUnansweredAsk(t *testing.T) {
	require.True(t, t.Run("held without answer", func(t *testing.T) {
		env := newQuestionInstall(t)
		owner, err := env.sessions.Get(t.Context(), env.sessionID)
		require.NoError(t, err)
		require.Empty(t, owner.ParentSessionID)
		env.submitAgentRoot()
		var published int
		require.Eventually(t, func() bool {
			return env.conn.QueryRowContext(t.Context(),
				`SELECT count(*) FROM session_notices WHERE owner = ?1 AND kind = ?2 AND wake = 1`,
				env.sessionID, session.NoticeKindChildQuestion).Scan(&published) == nil && published == 1
		}, 5*time.Second, time.Millisecond)
		// Возврат без ответа должен отменить Ask до ожидания runWg.
	}))
}
