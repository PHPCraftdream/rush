package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAsyncNoticeFlagsSurviveDurableRebuild(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	sess, err := env.sessions.Create(t.Context(), "async-notice")
	require.NoError(t, err)

	original := SessionAgentCall{
		SessionID:           sess.ID,
		Prompt:              "Async job call-1 (bash) finished.\n\ndone",
		Origin:              message.OriginWeb,
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
	raw, err := json.Marshal(ToSessionAgentCallData(original))
	require.NoError(t, err)
	var data session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &data))
	rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err)
	require.True(t, rebuilt.AutoResumed)
	require.True(t, rebuilt.BackgroundJobNotice)

	a := &sessionAgent{messages: env.messages}
	created, err := a.createUserMessage(context.Background(), rebuilt)
	require.NoError(t, err)
	persisted, err := env.messages.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, persisted.AutoResumed)
	require.True(t, persisted.BackgroundJobNotice)
}
