package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
)

func quietFinalizationTestFacts(context.Context) (gate.Facts, error) {
	return gate.Facts{Complete: true}, nil
}

func requireFinalizationCancelled(t *testing.T, g *gate.Gate, want bool, msgAndArgs ...any) {
	t.Helper()
	_, _, err := g.WaitState(t.Context(), quietFinalizationTestFacts)
	if want {
		require.ErrorIs(t, err, context.Canceled, msgAndArgs...)
	} else {
		require.NoError(t, err, msgAndArgs...)
	}
}

func requireFinalizationSealed(t *testing.T, g *gate.Gate, want bool) {
	t.Helper()
	verdict, _, _ := g.WaitState(t.Context(), quietFinalizationTestFacts)
	require.Equal(t, want, verdict == gate.Accepted)
}
