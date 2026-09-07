package mcp

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAcquireReclaimsEmptyImplicitOwnerWorkers(t *testing.T) {
	if current := currentOwner(); current != nil {
		require.NoError(t, current.Close(context.Background()))
	}
	t.Cleanup(func() {
		if current := currentOwner(); current != nil {
			_ = current.Close(context.Background())
		}
	})

	for iteration := 0; iteration < 4; iteration++ {
		store := config.NewTestStore(&config.Config{MCP: config.MCPs{}})
		if iteration%2 == 0 {
			Initialize(context.Background(), nil, store, false)
		} else {
			require.Error(t, InitializeSingle(context.Background(), "missing", store))
		}

		old := currentOwner()
		require.NotNil(t, old)
		require.True(t, old.implicit)
		refreshDone := old.refreshDone
		closerDone := old.closer.done
		fallbackDone := old.fallbackWorker.done

		acquired, err := Acquire()
		require.NoError(t, err)
		require.NotSame(t, old, acquired)
		require.Same(t, acquired, currentOwner())
		assertClosedWorker(t, refreshDone)
		assertClosedWorker(t, closerDone)
		assertClosedWorker(t, fallbackDone)
		assertClosedWorker(t, old.closeDone)
		assertClosedWorker(t, old.initDone)
		require.NoError(t, acquired.Close(context.Background()))
	}
}

func TestConcurrentAcquireReclaimsImplicitOwnerOnce(t *testing.T) {
	if current := currentOwner(); current != nil {
		require.NoError(t, current.Close(context.Background()))
	}
	t.Cleanup(func() {
		if current := currentOwner(); current != nil {
			_ = current.Close(context.Background())
		}
	})

	Initialize(context.Background(), nil, config.NewTestStore(&config.Config{MCP: config.MCPs{}}), false)
	old := currentOwner()
	require.NotNil(t, old)

	const contenders = 8
	start := make(chan struct{})
	type acquireResult struct {
		owner *Owner
		err   error
	}
	results := make(chan acquireResult, contenders)
	for range contenders {
		go func() {
			<-start
			acquired, err := Acquire()
			results <- acquireResult{owner: acquired, err: err}
		}()
	}
	close(start)

	var winner *Owner
	for range contenders {
		result := <-results
		acquired, err := result.owner, result.err
		if err == nil {
			require.Nil(t, winner)
			winner = acquired
		} else {
			require.ErrorIs(t, err, ErrOwnerBusy)
		}
	}
	require.NotNil(t, winner)
	require.NotSame(t, old, winner)
	require.Same(t, winner, currentOwner())
	require.NoError(t, winner.Close(context.Background()))
}

func assertClosedWorker(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal("worker did not finish before owner reclaim returned")
	}
}
