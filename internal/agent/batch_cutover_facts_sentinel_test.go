// R12-H fix regression guards for the unknown fail-safe's terminal sentinel:
// the flag-ON fallback (no binding) must answer conservatively AND surface
// the exported ErrBatchFactsSourceUnavailable, so callers (the rush run CLI
// loop, the reviewer-pass gate) can treat the condition as terminal instead
// of retrying a wiring defect. No t.Parallel(): shared package state.
package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnknownBatchCutoverFactsLiveScopeSentinel(t *testing.T) {
	require.NotNil(t, ErrBatchFactsSourceUnavailable)

	out, err := unknownBatchCutoverFacts{}.LiveScopeForOwners(context.Background(), []string{"s"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrBatchFactsSourceUnavailable)
	require.Len(t, out, 1)
	require.Equal(t, "s", out[0].Owner)
	require.True(t, out[0].Live, "unknown fail-safe stays conservatively live")
}
