package app

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchCLISpecEnforcesSessionExecutionRequirement(t *testing.T) {
	ctx := context.Background()
	a := newFullNewApp(t)
	sess, err := a.Sessions.Create(ctx, "batch execution requirement")
	require.NoError(t, err)
	reqStore := fenceLoader(t, a)
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeSequential, Items: []jobbatch.Spec{
		{Task: &jobbatch.TaskSpec{Payload: mustJSON(t, map[string]any{"tool": "run_command", "input": map[string]any{"program": "echo", "args": []string{"ok"}}})}},
	}}}

	t.Run("unfenced session is accepted", func(t *testing.T) {
		translated, release, err := a.BatchCLISpec(ctx, sess.ID, "message-unfenced", spec)
		require.NoError(t, err)
		require.NotNil(t, translated.Batch)
		require.NotNil(t, release)
		release()
	})

	t.Run("persistent custom-provider requirement is refused", func(t *testing.T) {
		require.NoError(t, reqStore.StoreSessionExecutionRequirement(ctx, sess.ID, session.SessionExecutionRequirement{BindingID: "persistent-custom-binding"}))
		_, release, err := a.BatchCLISpec(ctx, sess.ID, "message-fenced", spec)
		if release != nil {
			release()
		}
		require.ErrorIs(t, err, agent.ErrDiskProviderUnavailable)
		require.ErrorContains(t, err, "session has a persistent custom-provider requirement")
	})
}
