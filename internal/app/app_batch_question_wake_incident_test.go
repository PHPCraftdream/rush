package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

func TestBatchInstallQuestionWakeUsesAppDispatcher(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	isolateAppNewTestEnv(t)
	a, _, _ := newBatchMaintenanceApp(t)
	var deps agent.BatchInstallDeps
	batchInstallWiringHook = func(d agent.BatchInstallDeps) { deps = d }
	t.Cleanup(func() { batchInstallWiringHook = nil })
	batchRuntimeConfigHook = func(cfg *BatchRuntimeConfig) {
		cfg.RunnerFactory = func() (batchRunner, error) { return nil, errors.New("dormant question-wake install") }
	}
	t.Cleanup(func() { batchRuntimeConfigHook = nil })
	require.NoError(t, a.installBatchRuntime(context.Background()))
	defer func() { require.False(t, a.ShutdownWithResult().Forced) }()
	require.NotNil(t, deps.QuestionWake, "installed AgentDeps must have a question wake callback")
	require.NotNil(t, a.batchTimeoutWake)
	wakes := pwAttachWakeRecorder(a.batchTimeoutWake)
	owner, err := a.Sessions.Create(context.Background(), "question wake owner")
	require.NoError(t, err)
	deps.QuestionWake(context.Background(), owner.ID)
	select {
	case rec := <-wakes:
		require.Equal(t, owner.ID, rec.owner)
		require.True(t, rec.fact)
	case <-time.After(pwWakeTimeout):
		t.Fatal("question wake dependency did not dispatch through the app wake dispatcher")
	}
}
