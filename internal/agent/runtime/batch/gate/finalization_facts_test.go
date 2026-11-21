package gate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalizationEveryUnsettledFactBlocksSuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		facts Facts
	}{
		{name: "own work", facts: Facts{Complete: true, Own: 1}},
		{name: "descendant", facts: Facts{Complete: true, Descendants: 1}},
		{name: "physical hold", facts: Facts{Complete: true, PhysicalHolds: 1}},
		{name: "reaction debt", facts: Facts{Complete: true, Debt: 1}},
		{name: "queued logical work", facts: Facts{Complete: true, QueuedCalls: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate := New(15)
			read := func(context.Context) (Facts, error) { return tc.facts, nil }
			stamp, err := gate.BeginCandidate(t.Context(), read)
			require.NoError(t, err)
			require.False(t, stamp.ready, "noisy facts must not arm a ready stamp")
			verdict, err := gate.TrySeal(t.Context(), stamp, Candidate{MessageID: "message", Hash: "hash"}, read, func(context.Context, Candidate) error {
				t.Error("unsettled scope was published as successful")
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, WaitWork, verdict)
			require.False(t, gate.sealed)
		})
	}
}
