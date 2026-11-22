package finalize

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	_ "github.com/joho/godotenv/autoload"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { testguard.Main(m) }

func quietFinalizationFacts(context.Context) (gate.Facts, error) {
	return gate.Facts{Complete: true}, nil
}

func finalizationCancelTestExecution(sessionID string, epoch uint64) *FinalizationExecution {
	return &FinalizationExecution{SessionID: sessionID, LogicalID: "logical", Gate: gate.New(epoch), Read: quietFinalizationFacts}
}

func requireFinalizationCancelled(t *testing.T, g *gate.Gate, want bool, msgAndArgs ...any) {
	t.Helper()
	_, _, err := g.WaitState(t.Context(), quietFinalizationFacts)
	if want {
		require.ErrorIs(t, err, context.Canceled, msgAndArgs...)
	} else {
		require.NoError(t, err, msgAndArgs...)
	}
}

func requireFinalizationSealed(t *testing.T, g *gate.Gate, want bool) {
	t.Helper()
	verdict, _, _ := g.WaitState(t.Context(), quietFinalizationFacts)
	require.Equal(t, want, verdict == gate.Accepted)
}

func TestFinalizationOnCancelHookWakesArmedWaitThroughRegistry(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 21)
	registry := NewFinalizationCancelRegistry()
	registry.Register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	release, err := execution.Gate.AdmitMutation(true)
	require.NoError(t, err)
	defer release()
	_, changed, err := execution.Gate.WaitState(t.Context(), execution.Read)
	require.NoError(t, err)
	require.NotNil(t, changed)
	hook("session")
	select {
	case <-changed:
	default:
		t.Fatal("the cancel hook did not wake the owner waiting in waitState")
	}
}

func TestFinalizationOnCancelHookDoubleCancelIsIdempotent(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 22)
	registry := NewFinalizationCancelRegistry()
	registry.Register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	hook("session")
	requireFinalizationCancelled(t, execution.Gate, true)
	hook("session")
	hook("session")
	requireFinalizationCancelled(t, execution.Gate, true)
	requireFinalizationSealed(t, execution.Gate, false)
}

func TestFinalizationOnCancelHookUnknownSessionIsInert(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 23)
	registry := NewFinalizationCancelRegistry()
	registry.Register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	hook("other")
	hook("")
	requireFinalizationCancelled(t, execution.Gate, false)
	hook("session")
	requireFinalizationCancelled(t, execution.Gate, true)
}

func TestFinalizationOnCancelHookNilDependenciesDoNotPanic(t *testing.T) {
	t.Parallel()
	hook := NewFinalizationOnCancelHook(nil)
	hook("session")
	hook("")
	var registry *FinalizationCancelRegistry
	registry.Register(nil)
	registry.Register(finalizationCancelTestExecution("", 24))
	registry.Register(&FinalizationExecution{SessionID: "session"})
	registry.Register(&FinalizationExecution{SessionID: "session", LogicalID: "logical"})
	registry.Unregister(nil)
	registry.cancelSession("session")
	registry.cancelSession("")
}

func TestFinalizationConcurrentCancelAndSealAgreeOnExactlyOneOutcome(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 25)
	registry := NewFinalizationCancelRegistry()
	registry.Register(execution)
	hook := NewFinalizationOnCancelHook(registry)
	stamp, err := execution.Gate.BeginCandidate(t.Context(), execution.Read)
	require.NoError(t, err)
	var published atomic.Int32
	sealErr := make(chan error, 1)
	go func() {
		_, err := execution.Gate.TrySeal(t.Context(), stamp, gate.Candidate{MessageID: "m", Hash: "h"}, execution.Read, func(context.Context, gate.Candidate) error {
			published.Add(1)
			return nil
		})
		sealErr <- err
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); hook("session") }()
	}
	wg.Wait()
	err = <-sealErr
	count := published.Load()
	if count == 0 {
		require.ErrorIs(t, err, context.Canceled)
		requireFinalizationCancelled(t, execution.Gate, true)
		requireFinalizationSealed(t, execution.Gate, false)
	} else {
		require.EqualValues(t, 1, count)
		requireFinalizationSealed(t, execution.Gate, true)
		requireFinalizationCancelled(t, execution.Gate, false)
	}
}

func TestFinalizationOnCancelHookSurvivesConcurrentRegistryTraffic(t *testing.T) {
	t.Parallel()
	execution := finalizationCancelTestExecution("session", 26)
	other := finalizationCancelTestExecution("session", 27)
	registry := NewFinalizationCancelRegistry()
	registry.Register(execution)
	registry.Register(other)
	hook := NewFinalizationOnCancelHook(registry)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); hook("session"); hook("unknown") }()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registry.Unregister(other)
			registry.Register(other)
			registry.cancelSession("session")
		}()
	}
	wg.Wait()
	requireFinalizationCancelled(t, execution.Gate, true)
	registry.Unregister(execution)
	registry.Unregister(other)
	registry.mu.Lock()
	remaining := len(registry.byKey)
	registry.mu.Unlock()
	require.Zero(t, remaining)
}

func TestFinalizationCancelRegistryLifecycleDoesNotLeak(t *testing.T) {
	// Test-only whitebox assertions stay in this internal package; no
	// production Len accessor is added for registry internals.
	t.Parallel()
	first := finalizationCancelTestExecution("first", 28)
	second := finalizationCancelTestExecution("second", 29)
	registry := NewFinalizationCancelRegistry()
	registry.Register(first)
	registry.Register(first)
	registry.mu.Lock()
	buckets, firstBucket := len(registry.byKey), len(registry.byKey["first"])
	registry.mu.Unlock()
	require.Equal(t, 1, buckets)
	require.Equal(t, 1, firstBucket)
	registry.Register(second)
	registry.cancelSession("first")
	requireFinalizationCancelled(t, first.Gate, true)
	requireFinalizationCancelled(t, second.Gate, false)
	registry.Unregister(first)
	registry.cancelSession("first")
	registry.mu.Lock()
	buckets = len(registry.byKey)
	registry.mu.Unlock()
	require.Equal(t, 1, buckets)
	registry.Register(second)
	registry.mu.Lock()
	secondBucket := len(registry.byKey["second"])
	registry.mu.Unlock()
	require.Equal(t, 1, secondBucket)
	registry.Register(finalizationCancelTestExecution("", 30))
	registry.mu.Lock()
	buckets = len(registry.byKey)
	registry.mu.Unlock()
	require.Equal(t, 1, buckets)
	registry.Unregister(second)
	registry.Unregister(second)
	registry.mu.Lock()
	buckets = len(registry.byKey)
	registry.mu.Unlock()
	require.Zero(t, buckets)
}
