package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/gate"
)

func quietFinalizationTestFacts(context.Context) (finalizationFacts, error) {
	return finalizationFacts{Complete: true}, nil
}

func requireFinalizationCancelled(t *testing.T, g *finalizationGate, want bool, msgAndArgs ...any) {
	t.Helper()
	_, _, err := g.WaitState(t.Context(), quietFinalizationTestFacts)
	if want {
		require.ErrorIs(t, err, context.Canceled, msgAndArgs...)
	} else {
		require.NoError(t, err, msgAndArgs...)
	}
}

func requireFinalizationSealed(t *testing.T, g *finalizationGate, want bool) {
	t.Helper()
	verdict, _, _ := g.WaitState(t.Context(), quietFinalizationTestFacts)
	require.Equal(t, want, verdict == finalizationAccepted)
}

func TestFinalizationGateSentinelIdentity(t *testing.T) {
	t.Parallel()
	if ErrFinalizationFactsUnavailable != gate.ErrFinalizationFactsUnavailable ||
		ErrFinalizationSealed != gate.ErrFinalizationSealed ||
		ErrFinalizationScopeChanged != gate.ErrFinalizationScopeChanged {
		t.Fatal("agent and gate must share the same finalization sentinels")
	}
}
