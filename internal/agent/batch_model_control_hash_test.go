package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestModelControlRunCredentialBackedSameCallRetry(t *testing.T) {
	env := newMarkerModelControlEnv(t)
	reg, err := env.registries.Resolve(env.registryID)
	require.NoError(t, err)
	ctx := withCallCredentials(context.Background(), escrowTestCreds("retry-secret"))
	inv := markerCaptureInvocation(env)

	start := make(chan struct{})
	type retryResult struct {
		root jobbatch.ID
		err  error
	}
	retries := make(chan retryResult, 2)
	for range 2 {
		go func() {
			<-start
			root, err := env.control.Run(ctx, inv, reg, markerCaptureSpec())
			retries <- retryResult{root: root, err: err}
		}()
	}
	close(start)
	var firstRoot jobbatch.ID
	for i := 0; i < 2; i++ {
		result := <-retries
		require.NoError(t, result.err, "concurrent same-call retry must attach")
		if firstRoot == "" {
			firstRoot = result.root
		} else {
			require.Equal(t, firstRoot, result.root, "initial same-call race must share one live root")
		}
	}
	require.NotEmpty(t, firstRoot)
	require.Len(t, env.executor.snapshotLaunches(), 1, "concurrent same-call retry must submit only one credential-backed batch")

	env.executor.letGo()
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	batch := env.install.RunnerHandle.batchFor(jobbatch.ID(firstRoot))
	require.NotNil(t, batch)
	_, err = batch.Wait(waitCtx)
	require.NoError(t, err)
	markerCaptureWaitEscrowRootGone(t, env.install.RunnerHandle.credentials, jobbatch.ID(firstRoot))
}
