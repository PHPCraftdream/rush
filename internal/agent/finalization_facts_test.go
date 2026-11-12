package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalizationEveryUnsettledFactBlocksSuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		facts finalizationFacts
	}{
		{name: "own work", facts: finalizationFacts{complete: true, own: 1}},
		{name: "descendant", facts: finalizationFacts{complete: true, descendants: 1}},
		{name: "physical hold", facts: finalizationFacts{complete: true, physicalHolds: 1}},
		{name: "reaction debt", facts: finalizationFacts{complete: true, debt: 1}},
		{name: "queued logical work", facts: finalizationFacts{complete: true, queuedCalls: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate := newFinalizationGate(15)
			read := func(context.Context) (finalizationFacts, error) { return tc.facts, nil }
			stamp, err := gate.beginCandidate(t.Context(), read)
			require.NoError(t, err)
			verdict, err := gate.trySeal(t.Context(), stamp, finalizationCandidate{messageID: "message", hash: "hash"}, read, func(context.Context, finalizationCandidate) error {
				t.Error("unsettled scope was published as successful")
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, finalizationWaitWork, verdict)
			require.False(t, gate.sealed)
		})
	}
}
